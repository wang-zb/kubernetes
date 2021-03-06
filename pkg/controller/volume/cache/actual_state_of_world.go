/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
Package cache implements data structures used by the attach/detach controller
to keep track of volumes, the nodes they are attached to, and the pods that
reference them.
*/
package cache

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/kubernetes/pkg/volume/util/operationexecutor"
	"k8s.io/kubernetes/pkg/volume/util/volumehelper"
)

// ActualStateOfWorld defines a set of thread-safe operations supported on
// the attach/detach controller's actual state of the world cache.
// This cache contains volumes->nodes i.e. a set of all volumes and the nodes
// the attach/detach controller believes are successfully attached.
// Note: This is distinct from the ActualStateOfWorld implemented by the kubelet
// volume manager. They both keep track of different objects. This contains
// attach/detach controller specific state.
type ActualStateOfWorld interface {
	// ActualStateOfWorld must implement the methods required to allow
	// operationexecutor to interact with it.
	operationexecutor.ActualStateOfWorldAttacherUpdater

	// AddVolumeNode adds the given volume and node to the underlying store
	// indicating the specified volume is attached to the specified node.
	// A unique volume name is generated from the volumeSpec and returned on
	// success.
	// If the volume/node combo already exists, the detachRequestedTime is reset
	// to zero.
	// If volumeSpec is not an attachable volume plugin, an error is returned.
	// If no volume with the name volumeName exists in the store, the volume is
	// added.
	// If no node with the name nodeName exists in list of attached nodes for
	// the specified volume, the node is added.
	AddVolumeNode(volumeSpec *volume.Spec, nodeName string) (api.UniqueVolumeName, error)

	// SetVolumeMountedByNode sets the MountedByNode value for the given volume
	// and node. When set to true this value indicates the volume is mounted by
	// the given node, indicating it may not be safe to detach.
	// If no volume with the name volumeName exists in the store, an error is
	// returned.
	// If no node with the name nodeName exists in list of attached nodes for
	// the specified volume, an error is returned.
	SetVolumeMountedByNode(volumeName api.UniqueVolumeName, nodeName string, mounted bool) error

	// MarkDesireToDetach returns the difference between the current time  and
	// the DetachRequestedTime for the given volume/node combo. If the
	// DetachRequestedTime is zero, it is set to the current time.
	// If no volume with the name volumeName exists in the store, an error is
	// returned.
	// If no node with the name nodeName exists in list of attached nodes for
	// the specified volume, an error is returned.
	MarkDesireToDetach(volumeName api.UniqueVolumeName, nodeName string) (time.Duration, error)

	// DeleteVolumeNode removes the given volume and node from the underlying
	// store indicating the specified volume is no longer attached to the
	// specified node.
	// If the volume/node combo does not exist, this is a no-op.
	// If after deleting the node, the specified volume contains no other child
	// nodes, the volume is also deleted.
	DeleteVolumeNode(volumeName api.UniqueVolumeName, nodeName string)

	// VolumeNodeExists returns true if the specified volume/node combo exists
	// in the underlying store indicating the specified volume is attached to
	// the specified node.
	VolumeNodeExists(volumeName api.UniqueVolumeName, nodeName string) bool

	// GetAttachedVolumes generates and returns a list of volumes/node pairs
	// reflecting which volumes are attached to which nodes based on the
	// current actual state of the world.
	GetAttachedVolumes() []AttachedVolume

	// GetAttachedVolumes generates and returns a list of volumes attached to
	// the specified node reflecting which volumes are attached to that node
	// based on the current actual state of the world.
	GetAttachedVolumesForNode(nodeName string) []AttachedVolume
}

// AttachedVolume represents a volume that is attached to a node.
type AttachedVolume struct {
	operationexecutor.AttachedVolume

	// MountedByNode indicates that this volume has been been mounted by the
	// node and is unsafe to detach.
	// The value is set and unset by SetVolumeMountedByNode(...).
	MountedByNode bool

	// DetachRequestedTime is used to capture the desire to detach this volume.
	// When the volume is newly created this value is set to time zero.
	// It is set to current time, when MarkDesireToDetach(...) is called, if it
	// was previously set to zero (other wise its value remains the same).
	// It is reset to zero on AddVolumeNode(...) calls.
	DetachRequestedTime time.Time
}

// NewActualStateOfWorld returns a new instance of ActualStateOfWorld.
func NewActualStateOfWorld(volumePluginMgr *volume.VolumePluginMgr) ActualStateOfWorld {
	return &actualStateOfWorld{
		attachedVolumes: make(map[api.UniqueVolumeName]attachedVolume),
		volumePluginMgr: volumePluginMgr,
	}
}

type actualStateOfWorld struct {
	// attachedVolumes is a map containing the set of volumes the attach/detach
	// controller believes to be successfully attached to the nodes it is
	// managing. The key in this map is the name of the volume and the value is
	// an object containing more information about the attached volume.
	attachedVolumes map[api.UniqueVolumeName]attachedVolume
	// volumePluginMgr is the volume plugin manager used to create volume
	// plugin objects.
	volumePluginMgr *volume.VolumePluginMgr
	sync.RWMutex
}

// The volume object represents a volume the the attach/detach controller
// believes to be successfully attached to a node it is managing.
type attachedVolume struct {
	// volumeName contains the unique identifier for this volume.
	volumeName api.UniqueVolumeName

	// spec is the volume spec containing the specification for this volume.
	// Used to generate the volume plugin object, and passed to attach/detach
	// methods.
	spec *volume.Spec

	// nodesAttachedTo is a map containing the set of nodes this volume has
	// successfully been attached to. The key in this map is the name of the
	// node and the value is a node object containing more information about
	// the node.
	nodesAttachedTo map[string]nodeAttachedTo
}

// The nodeAttachedTo object represents a node that .
type nodeAttachedTo struct {
	// nodeName contains the name of this node.
	nodeName string

	// mountedByNode indicates that this node/volume combo is mounted by the
	// node and is unsafe to detach
	mountedByNode bool

	// number of times SetVolumeMountedByNode has been called to set the value
	// of mountedByNode to true. This is used to prevent mountedByNode from
	// being reset during the period between attach and mount when volumesInUse
	// status for the node may not be set.
	mountedByNodeSetCount uint

	// detachRequestedTime used to capture the desire to detach this volume
	detachRequestedTime time.Time
}

func (asw *actualStateOfWorld) MarkVolumeAsAttached(
	volumeSpec *volume.Spec, nodeName string) error {
	_, err := asw.AddVolumeNode(volumeSpec, nodeName)
	return err
}

func (asw *actualStateOfWorld) MarkVolumeAsDetached(
	volumeName api.UniqueVolumeName, nodeName string) {
	asw.DeleteVolumeNode(volumeName, nodeName)
}

func (asw *actualStateOfWorld) AddVolumeNode(
	volumeSpec *volume.Spec, nodeName string) (api.UniqueVolumeName, error) {
	asw.Lock()
	defer asw.Unlock()

	attachableVolumePlugin, err := asw.volumePluginMgr.FindAttachablePluginBySpec(volumeSpec)
	if err != nil || attachableVolumePlugin == nil {
		return "", fmt.Errorf(
			"failed to get AttachablePlugin from volumeSpec for volume %q err=%v",
			volumeSpec.Name(),
			err)
	}

	volumeName, err := volumehelper.GetUniqueVolumeNameFromSpec(
		attachableVolumePlugin, volumeSpec)
	if err != nil {
		return "", fmt.Errorf(
			"failed to GetUniqueVolumeNameFromSpec for volumeSpec %q err=%v",
			volumeSpec.Name(),
			err)
	}

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		volumeObj = attachedVolume{
			volumeName:      volumeName,
			spec:            volumeSpec,
			nodesAttachedTo: make(map[string]nodeAttachedTo),
		}
		asw.attachedVolumes[volumeName] = volumeObj
	}

	nodeObj, nodeExists := volumeObj.nodesAttachedTo[nodeName]
	if !nodeExists {
		// Create object if it doesn't exist.
		volumeObj.nodesAttachedTo[nodeName] = nodeAttachedTo{
			nodeName:              nodeName,
			mountedByNode:         true, // Assume mounted, until proven otherwise
			mountedByNodeSetCount: 0,
			detachRequestedTime:   time.Time{},
		}
	} else if !nodeObj.detachRequestedTime.IsZero() {
		// Reset detachRequestedTime values if object exists and time is non-zero
		nodeObj.detachRequestedTime = time.Time{}
		volumeObj.nodesAttachedTo[nodeName] = nodeObj
	}

	return volumeName, nil
}

func (asw *actualStateOfWorld) SetVolumeMountedByNode(
	volumeName api.UniqueVolumeName, nodeName string, mounted bool) error {
	asw.Lock()
	defer asw.Unlock()
	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return fmt.Errorf(
			"failed to SetVolumeMountedByNode(volumeName=%v, nodeName=%q, mounted=%v) volumeName does not exist",
			volumeName,
			nodeName,
			mounted)
	}

	nodeObj, nodeExists := volumeObj.nodesAttachedTo[nodeName]
	if !nodeExists {
		return fmt.Errorf(
			"failed to SetVolumeMountedByNode(volumeName=%v, nodeName=%q, mounted=%v) nodeName does not exist",
			volumeName,
			nodeName,
			mounted)
	}

	if mounted {
		// Increment set count
		nodeObj.mountedByNodeSetCount = nodeObj.mountedByNodeSetCount + 1
	} else {
		// Do not allow value to be reset unless it has been set at least once
		if nodeObj.mountedByNodeSetCount == 0 {
			return nil
		}
	}

	nodeObj.mountedByNode = mounted
	volumeObj.nodesAttachedTo[nodeName] = nodeObj

	return nil
}

func (asw *actualStateOfWorld) MarkDesireToDetach(
	volumeName api.UniqueVolumeName, nodeName string) (time.Duration, error) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return time.Millisecond * 0, fmt.Errorf(
			"failed to MarkDesireToDetach(volumeName=%v, nodeName=%q) volumeName does not exist",
			volumeName,
			nodeName)
	}

	nodeObj, nodeExists := volumeObj.nodesAttachedTo[nodeName]
	if !nodeExists {
		return time.Millisecond * 0, fmt.Errorf(
			"failed to MarkDesireToDetach(volumeName=%v, nodeName=%q) nodeName does not exist",
			volumeName,
			nodeName)
	}

	if nodeObj.detachRequestedTime.IsZero() {
		nodeObj.detachRequestedTime = time.Now()
		volumeObj.nodesAttachedTo[nodeName] = nodeObj
	}

	return time.Since(volumeObj.nodesAttachedTo[nodeName].detachRequestedTime), nil
}

func (asw *actualStateOfWorld) DeleteVolumeNode(
	volumeName api.UniqueVolumeName, nodeName string) {
	asw.Lock()
	defer asw.Unlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if !volumeExists {
		return
	}

	_, nodeExists := volumeObj.nodesAttachedTo[nodeName]
	if nodeExists {
		delete(asw.attachedVolumes[volumeName].nodesAttachedTo, nodeName)
	}

	if len(volumeObj.nodesAttachedTo) == 0 {
		delete(asw.attachedVolumes, volumeName)
	}
}

func (asw *actualStateOfWorld) VolumeNodeExists(
	volumeName api.UniqueVolumeName, nodeName string) bool {
	asw.RLock()
	defer asw.RUnlock()

	volumeObj, volumeExists := asw.attachedVolumes[volumeName]
	if volumeExists {
		if _, nodeExists := volumeObj.nodesAttachedTo[nodeName]; nodeExists {
			return true
		}
	}

	return false
}

func (asw *actualStateOfWorld) GetAttachedVolumes() []AttachedVolume {
	asw.RLock()
	defer asw.RUnlock()

	attachedVolumes := make([]AttachedVolume, 0 /* len */, len(asw.attachedVolumes) /* cap */)
	for _, volumeObj := range asw.attachedVolumes {
		for _, nodeObj := range volumeObj.nodesAttachedTo {
			attachedVolumes = append(
				attachedVolumes,
				getAttachedVolume(&volumeObj, &nodeObj))
		}
	}

	return attachedVolumes
}

func (asw *actualStateOfWorld) GetAttachedVolumesForNode(
	nodeName string) []AttachedVolume {
	asw.RLock()
	defer asw.RUnlock()

	attachedVolumes := make(
		[]AttachedVolume, 0 /* len */, len(asw.attachedVolumes) /* cap */)
	for _, volumeObj := range asw.attachedVolumes {
		for actualNodeName, nodeObj := range volumeObj.nodesAttachedTo {
			if actualNodeName == nodeName {
				attachedVolumes = append(
					attachedVolumes,
					getAttachedVolume(&volumeObj, &nodeObj))
			}
		}
	}

	return attachedVolumes
}

func getAttachedVolume(
	attachedVolume *attachedVolume,
	nodeAttachedTo *nodeAttachedTo) AttachedVolume {
	return AttachedVolume{
		AttachedVolume: operationexecutor.AttachedVolume{
			VolumeName:         attachedVolume.volumeName,
			VolumeSpec:         attachedVolume.spec,
			NodeName:           nodeAttachedTo.nodeName,
			PluginIsAttachable: true,
		},
		MountedByNode:       nodeAttachedTo.mountedByNode,
		DetachRequestedTime: nodeAttachedTo.detachRequestedTime}
}
