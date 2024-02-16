/*
Copyright 2018 The Kubernetes Authors.
Copyright 2022 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/util"
	"github.com/googlecloudplatform/gcs-fuse-csi-driver/pkg/webhook"
	pbSanitizer "github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

const (
	CreateVolumeCSIFullMethod      = "/csi.v1.Controller/CreateVolume"
	DeleteVolumeCSIFullMethod      = "/csi.v1.Controller/DeleteVolume"
	NodePublishVolumeCSIFullMethod = "/csi.v1.Node/NodePublishVolume"

	VolumeContextKeyMountOptions     = "mountOptions"
	VolumeContextKeyDisableFileCache = "disableFileCache"
)

func NewVolumeCapabilityAccessMode(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability_AccessMode {
	return &csi.VolumeCapability_AccessMode{Mode: mode}
}

func NewControllerServiceCapability(c csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
	return &csi.ControllerServiceCapability{
		Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{
				Type: c,
			},
		},
	}
}

func NewNodeServiceCapability(c csi.NodeServiceCapability_RPC_Type) *csi.NodeServiceCapability {
	return &csi.NodeServiceCapability{
		Type: &csi.NodeServiceCapability_Rpc{
			Rpc: &csi.NodeServiceCapability_RPC{
				Type: c,
			},
		},
	}
}

func logGRPC(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	var strippedReq string
	switch info.FullMethod {
	case CreateVolumeCSIFullMethod:
		strippedReq = pbSanitizer.StripSecrets(req).String()
	case DeleteVolumeCSIFullMethod:
		strippedReq = pbSanitizer.StripSecrets(req).String()
	case NodePublishVolumeCSIFullMethod:
		if nodePublishReq, ok := req.(*csi.NodePublishVolumeRequest); ok {
			if token, ok := nodePublishReq.GetVolumeContext()[VolumeContextKeyServiceAccountToken]; ok {
				nodePublishReq.VolumeContext[VolumeContextKeyServiceAccountToken] = "***stripped***"
				strippedReq = fmt.Sprintf("%+v", nodePublishReq)
				nodePublishReq.VolumeContext[VolumeContextKeyServiceAccountToken] = token
			} else {
				strippedReq = fmt.Sprintf("%+v", req)
			}
		} else {
			klog.Errorf("failed to case req to *csi.NodePublishVolumeRequest")
		}
	default:
		strippedReq = fmt.Sprintf("%+v", req)
	}

	klog.V(4).Infof("%s called with request: %v", info.FullMethod, strippedReq)
	resp, err := handler(ctx, req)
	if err != nil {
		klog.Errorf("%s failed with error: %v", info.FullMethod, err)
	} else {
		if fmt.Sprintf("%v", resp) == "" {
			klog.V(4).Infof("%s succeeded.", info.FullMethod)
		} else {
			klog.V(4).Infof("%s succeeded with response: %s", info.FullMethod, resp)
		}
	}

	return resp, err
}

// joinMountOptions joins mount options eliminating duplicates.
func joinMountOptions(existingOptions []string, newOptions []string) []string {
	overwritableOptions := map[string]string{
		"gid":       "",
		"file-mode": "",
		"dir-mode":  "",
	}

	allMountOptions := sets.NewString()

	process := func(mountOption string) {
		if len(mountOption) > 0 {
			optionPair := strings.SplitN(mountOption, "=", 2)

			if len(optionPair) == 2 {
				if _, ok := overwritableOptions[optionPair[0]]; ok {
					overwritableOptions[optionPair[0]] = optionPair[1]

					return
				}
			}

			allMountOptions.Insert(mountOption)
		}
	}

	for _, mountOption := range existingOptions {
		process(mountOption)
	}

	for _, mountOption := range newOptions {
		process(mountOption)
	}

	for k, v := range overwritableOptions {
		if v != "" {
			allMountOptions.Insert(k + "=" + v)
		}
	}

	return allMountOptions.List()
}

// parseVolumeAttributes parses volume attributes and convert them to gcsfuse mount options.
func parseVolumeAttributes(fuseMountOptions []string, volumeContext map[string]string) []string {
	if mountOptions, ok := volumeContext[VolumeContextKeyMountOptions]; ok {
		fuseMountOptions = joinMountOptions(fuseMountOptions, strings.Split(mountOptions, ","))
	}

	if disableFileCache, ok := volumeContext[VolumeContextKeyDisableFileCache]; ok {
		if boolVal, err := strconv.ParseBool(disableFileCache); err == nil && boolVal {
			fuseMountOptions = joinMountOptions(fuseMountOptions, []string{util.DisableFileCacheKey})
		}
	}

	return fuseMountOptions
}

func putExitFile(pod *v1.Pod, emptyDirBasePath string) error {
	podIsTerminating := pod.DeletionTimestamp != nil
	podRestartPolicyIsNever := pod.Spec.RestartPolicy == v1.RestartPolicyNever
	podRestartPolicyIsOnFailure := pod.Spec.RestartPolicy == v1.RestartPolicyOnFailure

	// Check if all the containers besides the sidecar container exited
	if podRestartPolicyIsOnFailure || podRestartPolicyIsNever || podIsTerminating {
		if pod.Status.ContainerStatuses == nil || len(pod.Status.ContainerStatuses) == 0 {
			return nil
		}

		for _, cs := range pod.Status.ContainerStatuses {
			switch {
			// skip the sidecar container itself
			case cs.Name == webhook.SidecarContainerName:
				continue

			// If the Pod is terminating, the container status from Kubernetes API is not reliable
			// because of the issue: https://github.com/kubernetes/kubernetes/issues/106896,
			// so container status checking is skipped.
			// Directly pulling the container status from CRI is not acceptable due to security concerns.
			// This will cause the issue https://github.com/GoogleCloudPlatform/gcs-fuse-csi-driver/issues/168.
			// The issue will be solved by the Kubernetes native sidecar container feature.
			case podIsTerminating:
				return nil

			// If any container is in Running or Waiting state,
			// do not terminate the gcsfuse sidecar container.
			case cs.State.Running != nil || cs.State.Waiting != nil:
				return nil

			// If the Pod RestartPolicy is OnFailure,
			// when the container terminated with a non-zero exit code,
			// the container may restart. Do not terminate the gcsfuse sidecar container.
			// When the Pod belongs to a Job, and the container restart count reaches the Job backoffLimit,
			// the Pod will be directly terminated, which goes to the first case.
			case podRestartPolicyIsOnFailure && cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0:
				return nil
			}
		}

		klog.V(4).Infof("[Pod %v/%v, UID %v] all the other containers terminated in the Pod, put the exit file.", pod.Namespace, pod.Name, pod.UID)
		exitFilePath := filepath.Dir(emptyDirBasePath) + "/exit"
		f, err := os.Create(exitFilePath)
		if err != nil {
			return fmt.Errorf("failed to put the exit file: %w", err)
		}
		f.Close()

		err = os.Chown(exitFilePath, webhook.NobodyUID, webhook.NobodyGID)
		if err != nil {
			return fmt.Errorf("failed to change ownership on the exit file: %w", err)
		}
	}

	return nil
}
