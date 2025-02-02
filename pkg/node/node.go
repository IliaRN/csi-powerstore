/*
 *
 * Copyright © 2021 Dell Inc. or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

// Package node provides CSI specification compatible node service.
package node

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/dell/csi-powerstore/pkg/array"
	"github.com/dell/csi-powerstore/pkg/common"
	"github.com/dell/csi-powerstore/pkg/common/fs"
	"github.com/dell/csi-powerstore/pkg/controller"
	"github.com/dell/gobrick"
	"github.com/dell/gofsutil"
	"github.com/dell/goiscsi"
	"github.com/dell/gopowerstore"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	k8sutilfs "k8s.io/kubernetes/pkg/volume/util/fs"
)

// Opts defines service configuration options.
type Opts struct {
	NodeIDFilePath        string
	NodeNamePrefix        string
	NodeChrootPath        string
	FCPortsFilterFilePath string
	KubeNodeName          string
	CHAPUsername          string
	CHAPPassword          string
	TmpDir                string
	EnableCHAP            bool
}

// Service is a controller service that contains scsi connectors and implements NodeServer API
type Service struct {
	Fs fs.Interface

	ctrlSvc        controller.Interface
	iscsiConnector ISCSIConnector
	fcConnector    FcConnector
	iscsiLib       goiscsi.ISCSIinterface

	opts   Opts
	nodeID string

	useFC       bool
	initialized bool
	reusedHost  bool

	array.Locker
}

// Init initializes node service by parsing environmental variables, connecting it as a host.
// Will init ISCSIConnector, FcConnector and ControllerService if they are nil.
func (s *Service) Init() error {
	s.opts = getNodeOptions()

	s.initConnectors()

	err := s.updateNodeID()
	if err != nil {
		return fmt.Errorf("can't update node id: %s", err.Error())
	}

	iscsiInitiators, fcInitiators, err := s.getInitiators()
	if err != nil {
		return fmt.Errorf("can't get initiators of the node: %s", err.Error())
	}

	if len(iscsiInitiators) == 0 && len(fcInitiators) == 0 {
		return nil
	}

	// Setup host on each of available arrays
	for _, arr := range s.Arrays() {
		if arr.BlockProtocol == common.NoneTransport {
			continue
		}

		var initiators []string

		switch arr.BlockProtocol {
		case common.ISCSITransport:
			if len(iscsiInitiators) == 0 {
				return fmt.Errorf("iSCSI transport was requested but iSCSI initiator is not available")
			}
			s.useFC = false
		case common.FcTransport:
			if len(fcInitiators) == 0 {
				return fmt.Errorf("FC transport was requested but FC initiator is not available")
			}
			s.useFC = true
		default:
			s.useFC = len(fcInitiators) > 0
		}

		if s.useFC {
			initiators = fcInitiators
		} else {
			initiators = iscsiInitiators
		}

		err = s.setupHost(initiators, arr.GetClient(), arr.GetIP())
		if err != nil {
			log.Errorf("can't setup host on %s: %s", arr.Endpoint, err.Error())
		}
	}

	return nil
}

func (s *Service) initConnectors() {
	gobrick.SetLogger(&common.CustomLogger{})

	if s.iscsiConnector == nil {
		s.iscsiConnector = gobrick.NewISCSIConnector(
			gobrick.ISCSIConnectorParams{
				Chroot:       s.opts.NodeChrootPath,
				ChapUser:     s.opts.CHAPUsername,
				ChapPassword: s.opts.CHAPPassword,
				ChapEnabled:  s.opts.EnableCHAP,
			})
	}

	if s.fcConnector == nil {
		s.fcConnector = gobrick.NewFCConnector(
			gobrick.FCConnectorParams{Chroot: s.opts.NodeChrootPath})
	}

	if s.ctrlSvc == nil {
		svc := &controller.Service{Fs: s.Fs}
		svc.SetArrays(s.Arrays())
		svc.SetDefaultArray(s.DefaultArray())
		s.ctrlSvc = svc
	}

	if s.iscsiLib == nil {
		iSCSIOpts := make(map[string]string)
		iSCSIOpts["chrootDirectory"] = s.opts.NodeChrootPath

		s.iscsiLib = goiscsi.NewLinuxISCSI(iSCSIOpts)
	}

}

// NodeStageVolume prepares volume to be consumed by node publish by connecting volume to the node
func (s *Service) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	logFields := common.GetLogFields(ctx)

	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}

	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	id, arrayID, protocol, _ := array.ParseVolumeID(ctx, id, s.DefaultArray(), req.VolumeCapability)

	var stager VolumeStager

	arr, ok := s.Arrays()[arrayID]
	if !ok {
		return nil, status.Errorf(codes.Internal, "can't find array with provided arrayID %s", arrayID)
	}

	if protocol == "nfs" {
		stager = &NFSStager{
			array: arr,
		}
	} else {
		stager = &SCSIStager{
			useFC:          s.useFC,
			iscsiConnector: s.iscsiConnector,
			fcConnector:    s.fcConnector,
		}
	}

	return stager.Stage(ctx, req, logFields, s.Fs, id)
}

// NodeUnstageVolume reverses steps done in NodeStage by disconnecting volume from the node
func (s *Service) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	logFields := common.GetLogFields(ctx)
	var err error

	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	id, _, protocol, err := array.ParseVolumeID(ctx, id, s.DefaultArray(), nil)
	if err != nil {
		if apiError, ok := err.(gopowerstore.APIError); ok && apiError.NotFound() {
			return &csi.NodeUnstageVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Unknown,
			"failure checking volume status for volume node unstage: %s",
			err.Error())
	}

	// append additional path to be able to do bind mounts
	stagingPath := getStagingPath(ctx, req.GetStagingTargetPath(), id)

	device, err := unstageVolume(ctx, stagingPath, id, logFields, err, s.Fs)
	if err != nil {
		return nil, err
	}

	if protocol == "nfs" {
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	if device != "" {
		err := createMapping(id, device, s.opts.TmpDir, s.Fs)
		if err != nil {
			log.WithFields(logFields).Warningf("failed to create vol to device mapping: %s", err.Error())
		}
	} else {
		device, err = getMapping(id, s.opts.TmpDir, s.Fs)
		if err != nil {
			log.WithFields(logFields).Info("no device found. skip device removal")
			return &csi.NodeUnstageVolumeResponse{}, nil
		}
	}

	f := log.Fields{"Device": device}

	connectorCtx := common.SetLogFields(context.Background(), logFields)

	if s.useFC {
		err = s.fcConnector.DisconnectVolumeByDeviceName(connectorCtx, device)
	} else {
		err = s.iscsiConnector.DisconnectVolumeByDeviceName(connectorCtx, device)
	}
	if err != nil {
		log.WithFields(logFields).Error(err)
		return nil, err
	}
	log.WithFields(logFields).WithFields(f).Info("block device removal complete")

	err = deleteMapping(id, s.opts.TmpDir, s.Fs)
	if err != nil {
		log.WithFields(logFields).Warningf("failed to remove vol to Dev mapping: %s", err.Error())
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func unstageVolume(ctx context.Context, stagingPath, id string, logFields log.Fields, err error, fs fs.Interface) (string, error) {
	logFields["ID"] = id
	logFields["StagingPath"] = stagingPath
	ctx = common.SetLogFields(ctx, logFields)

	log.WithFields(logFields).Info("calling unstage")

	device, err := getStagedDev(ctx, stagingPath, fs)
	if err != nil {
		return "", status.Errorf(codes.Internal,
			"could not reliably determine existing mount for path %s: %s", stagingPath, err.Error())
	}

	if device != "" {
		_, device = path.Split(device)
		log.WithFields(logFields).Infof("active mount exist")
		err = fs.GetUtil().Unmount(ctx, stagingPath)
		if err != nil {
			return "", status.Errorf(codes.Internal,
				"could not unmount dev %s: %s", device, err.Error())
		}
		log.WithFields(logFields).Infof("unmount without error")
	} else {
		// no mounts
		log.WithFields(logFields).Infof("no mounts found")
	}

	err = fs.Remove(stagingPath)
	if err != nil && !fs.IsNotExist(err) {
		return "", status.Errorf(codes.Internal, "failed to delete mount path %s: %s", stagingPath, err.Error())
	}

	log.WithFields(logFields).Infof("target mount file deleted")
	return device, nil
}

// NodePublishVolume publishes volume to the node by mounting it to the target path
func (s *Service) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	logFields := common.GetLogFields(ctx)
	var ephemeralVolume bool
	ephemeral, ok := req.VolumeContext["csi.storage.k8s.io/ephemeral"]
	if ok {
		ephemeralVolume = strings.ToLower(ephemeral) == "true"
	}

	if ephemeralVolume {
		return s.ephemeralNodePublish(ctx, req)
	}
	// Get the VolumeID and validate against the volume
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "targetPath is required")
	}

	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeCapability is required")
	}

	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "stagingPath is required")
	}

	id, _, protocol, _ := array.ParseVolumeID(ctx, id, s.DefaultArray(), req.VolumeCapability)

	// append additional path to be able to do bind mounts
	stagingPath := getStagingPath(ctx, req.GetStagingTargetPath(), id)

	isRO := req.GetReadonly()
	volumeCapability := req.GetVolumeCapability()

	logFields["ID"] = id
	logFields["TargetPath"] = targetPath
	logFields["StagingPath"] = stagingPath
	logFields["ReadOnly"] = req.GetReadonly()
	ctx = common.SetLogFields(ctx, logFields)

	log.WithFields(logFields).Info("calling publish")

	var publisher VolumePublisher

	if protocol == "nfs" {
		if s.fileExists(filepath.Join(stagingPath, commonNfsVolumeFolder)) {
			// Assume root squashing is enabled
			stagingPath = filepath.Join(stagingPath, commonNfsVolumeFolder)
		}

		publisher = &NFSPublisher{}
	} else {
		publisher = &SCSIPublisher{
			isBlock: isBlock(req.VolumeCapability),
		}
	}

	return publisher.Publish(ctx, logFields, s.Fs, volumeCapability, isRO, targetPath, stagingPath)
}

// NodeUnpublishVolume unpublishes volume from the node by unmounting it from the target path
func (s *Service) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	logFields := common.GetLogFields(ctx)
	var err error

	targetPath := req.GetTargetPath()
	if targetPath == "" {
		log.Error("target path required")
		return nil, status.Error(codes.InvalidArgument, "target path required")
	}
	volID := req.GetVolumeId()
	if volID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	var ephemeralVolume bool
	lockFile := ephemeralStagingMountPath + volID + "/id"

	if s.fileExists(lockFile) {
		ephemeralVolume = true
	}
	logFields["ID"] = volID
	logFields["TargetPath"] = targetPath
	ctx = common.SetLogFields(ctx, logFields)
	log.WithFields(logFields).Info("calling unpublish")

	_, found, err := getTargetMount(ctx, targetPath, s.Fs)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"could not reliably determine existing mount status for path %s: %s",
			targetPath, err.Error())
	}

	if !found {
		// no mounts
		log.WithFields(logFields).Infof("no mounts found")
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	log.WithFields(logFields).Infof("active mount exist")
	err = s.Fs.GetUtil().Unmount(ctx, targetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"could not unmount dev %s: %s",
			targetPath, err.Error())
	}

	log.WithFields(logFields).Info("unpublish complete")
	log.Debug("Checking for ephemeral after node unpublish")

	if ephemeralVolume {
		log.Info("Detected ephemeral")
		err = s.ephemeralNodeUnpublish(ctx, req)
		if err != nil {
			return nil, err
		}

	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetVolumeStats returns volume usage stats
func (s *Service) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no volume ID provided")
	}

	volumePath := req.GetVolumePath()
	if len(volumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no volume Path provided")
	}

	// parse volume Id
	id, arrayID, protocol, err := array.ParseVolumeID(ctx, volumeID, s.DefaultArray(), nil)
	if err != nil {
		if apiError, ok := err.(gopowerstore.APIError); ok && apiError.NotFound() {
			return nil, err
		}
		return nil, err
	}

	arr, ok := s.Arrays()[arrayID]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "failed to find array with given ID")
	}

	// Validate if volume exists
	if protocol == "nfs" {
		_, err = arr.Client.GetFS(ctx, id)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "volume with ID '%s' not found", id)
		}
	} else {
		_, err := arr.Client.GetVolume(ctx, id)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "volume with ID '%s' not found", id)
		}
	}

	// Check if target path is mounted
	_, found, err := getTargetMount(ctx, volumePath, s.Fs)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "can't check mounts for path %s: %s", volumePath, err.Error())
	}
	if !found {
		resp := &csi.NodeGetVolumeStatsResponse{
			VolumeCondition: &csi.VolumeCondition{
				Abnormal: true,
				Message:  "volume path not mounted",
			},
		}
		return resp, nil
	}

	// check if volume path is accessible
	_, err = ioutil.ReadDir(volumePath)
	if err != nil {
		resp := &csi.NodeGetVolumeStatsResponse{
			VolumeCondition: &csi.VolumeCondition{
				Abnormal: true,
				Message:  "volume path not accessible",
			},
		}
		return resp, nil
	}

	// get volume metrics for mounted volume path
	availableBytes, totalBytes, usedBytes, totalInodes, freeInodes, usedInodes, err := k8sutilfs.Info(volumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get metrics for volume with error: %v", err)
	}

	resp := &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Available: availableBytes,
				Total:     totalBytes,
				Used:      usedBytes,
				Unit:      csi.VolumeUsage_BYTES,
			},
			{
				Available: freeInodes,
				Total:     totalInodes,
				Used:      usedInodes,
				Unit:      csi.VolumeUsage_INODES,
			},
		},
		VolumeCondition: &csi.VolumeCondition{
			Abnormal: false,
			Message:  "",
		},
	}

	return resp, nil
}

// NodeExpandVolume expands the volume by re-scanning and resizes filesystem if needed
func (s *Service) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	var reqID string
	var err error
	headers, ok := metadata.FromIncomingContext(ctx)
	if ok {
		if req, ok := headers["csi.requestid"]; ok && len(req) > 0 {
			reqID = req[0]
		}
	}

	// Get the VolumeID and validate against the volume
	id, arrayID, _, err := array.ParseVolumeID(ctx, req.VolumeId, s.DefaultArray(), nil)
	if err != nil {
		if apiError, ok := err.(gopowerstore.APIError); ok && apiError.NotFound() {
			return nil, err
		}
		return nil, err
	}

	arr, ok := s.Arrays()[arrayID]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "failed to find array with given ID")
	}

	targetPath := req.GetVolumePath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "targetPath is required")
	}
	isBlock := strings.Contains(targetPath, blockVolumePathMarker)

	// Parse the CSI VolumeId and validate against the volume
	vol, err := arr.Client.GetVolume(ctx, id)
	if err != nil {
		// If the volume isn't found, we cannot stage it
		return nil, status.Error(codes.NotFound, "Volume not found")
	}
	volumeWWN := vol.Wwn

	// Locate and fetch all (multipath/regular) mounted paths using this volume
	var devMnt *gofsutil.DeviceMountInfo
	var targetmount string
	devMnt, err = s.Fs.GetUtil().GetMountInfoFromDevice(ctx, vol.Name)
	if err != nil {
		if isBlock {
			return s.nodeExpandRawBlockVolume(ctx, volumeWWN)
		}
		log.Infof("Failed to find mount info for (%s) with error (%s)", vol.Name, err.Error())
		log.Info("Probably offline volume expansion. Will try to perform a temporary mount.")
		var disklocation string

		disklocation = fmt.Sprintf("%s/%s", targetPath, vol.ID)
		log.Infof("DisklLocation: %s", disklocation)
		targetmount = fmt.Sprintf("tmp/%s/%s", vol.ID, vol.Name)
		log.Infof("TargetMount: %s", targetmount)
		err = s.Fs.MkdirAll(targetmount, 0750)
		if err != nil {
			return nil, status.Error(codes.Internal,
				fmt.Sprintf("Failed to find mount info for (%s) with error (%s)", vol.Name, err.Error()))
		}
		err = s.Fs.GetUtil().Mount(ctx, disklocation, targetmount, "")
		if err != nil {
			return nil, status.Error(codes.Internal,
				fmt.Sprintf("Failed to find mount info for (%s) with error (%s)", vol.Name, err.Error()))
		}

		defer func() {
			if targetmount != "" {
				log.Infof("Clearing down temporary mount points in: %s", targetmount)
				err := s.Fs.GetUtil().Unmount(ctx, targetmount)
				if err != nil {
					log.Error("Failed to remove temporary mount points")
				}
				err = s.Fs.RemoveAll(targetmount)
				if err != nil {
					log.Error("Failed to remove temporary mount points")
				}
			}
		}()

		devMnt, err = s.Fs.GetUtil().GetMountInfoFromDevice(ctx, vol.Name)
		if err != nil {
			return nil, status.Error(codes.Internal,
				fmt.Sprintf("Failed to find mount info for (%s) with error (%s)", vol.Name, err.Error()))
		}

	}

	log.Infof("Mount info for volume %s: %+v", vol.Name, devMnt)

	size := req.GetCapacityRange().GetRequiredBytes()

	f := log.Fields{
		"CSIRequestID": reqID,
		"VolumeName":   vol.Name,
		"VolumePath":   targetPath,
		"Size":         size,
		"VolumeWWN":    volumeWWN,
	}
	log.WithFields(f).Info("Calling resize the file system")

	// Rescan the device for the volume expanded on the array
	for _, device := range devMnt.DeviceNames {
		devicePath := sysBlock + device
		err = s.Fs.GetUtil().DeviceRescan(context.Background(), devicePath)
		if err != nil {
			log.Errorf("Failed to rescan device (%s) with error (%s)", devicePath, err.Error())
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	// Expand the filesystem with the actual expanded volume size.
	if devMnt.MPathName != "" {
		err = s.Fs.GetUtil().ResizeMultipath(context.Background(), devMnt.MPathName)
		if err != nil {
			log.Errorf("Failed to resize filesystem: device  (%s) with error (%s)", devMnt.MountPoint, err.Error())

			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	// For a regular device, get the device path (devMnt.DeviceNames[1]) where the filesystem is mounted
	// PublishVolume creates devMnt.DeviceNames[0] but is left unused for regular devices
	var devicePath string
	if len(devMnt.DeviceNames) > 1 {
		devicePath = "/dev/" + devMnt.DeviceNames[1]
	} else if len(devMnt.DeviceNames) == 0 {
		return nil, status.Error(codes.Internal,
			fmt.Sprintf("Failed to find mount info for (%s) DeviceNames (%v)", vol.Name, devMnt.DeviceNames))
	} else {
		devicePath = "/dev/" + devMnt.DeviceNames[0]
	}
	fsType, err := s.Fs.GetUtil().FindFSType(context.Background(), devMnt.MountPoint)
	if err != nil {
		log.Errorf("Failed to fetch filesystem for volume  (%s) with error (%s)", devMnt.MountPoint, err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}
	log.Infof("Found %s filesystem mounted on volume %s", fsType, devMnt.MountPoint)
	// Resize the filesystem
	var xfsNew bool
	checkVersCmd := "xfs_growfs -V"
	bufcheck, errcheck := s.Fs.ExecCommandOutput("bash", "-c", checkVersCmd)
	if errcheck != nil {
		return nil, errcheck
	}
	outputcheck := string(bufcheck)
	versionRegx := regexp.MustCompile(`version (?P<versmaj>\d+)\.(?P<versmin>\d+)\..+`)
	match := versionRegx.FindStringSubmatch(outputcheck)
	subMatchMap := make(map[string]string)
	for i, name := range versionRegx.SubexpNames() {
		if i != 0 {
			subMatchMap[name] = match[i]
		}
	}

	if s, err := strconv.ParseFloat(subMatchMap["versmaj"]+"."+subMatchMap["versmin"], 64); err == nil {
		fmt.Println(s)
		if s >= 5.0 { // need to check exact version
			xfsNew = true
		} else {
			xfsNew = false
		}
	} else {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if fsType == "xfs" && xfsNew {
		err = s.Fs.GetUtil().ResizeFS(context.Background(), devMnt.MountPoint, devicePath, "", fsType)
		if err != nil {
			log.Errorf("Failed to resize filesystem: mountpoint (%s) device (%s) with error (%s)",
				devMnt.MountPoint, devicePath, err.Error())
			return nil, status.Error(codes.Internal, err.Error())
		}
	} else {
		err = s.Fs.GetUtil().ResizeFS(context.Background(), devMnt.MountPoint, devicePath, devMnt.MPathName, fsType)
		if err != nil {
			log.Errorf("Failed to resize filesystem: mountpoint (%s) device (%s) with error (%s)",
				devMnt.MountPoint, devicePath, err.Error())
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

func (s *Service) nodeExpandRawBlockVolume(ctx context.Context, volumeWWN string) (*csi.NodeExpandVolumeResponse, error) {
	log.Info(" Block volume expansion. Will try to perform a rescan...")
	wwnNum := strings.Replace(volumeWWN, "naa.", "", 1)
	deviceNames, err := s.Fs.GetUtil().GetSysBlockDevicesForVolumeWWN(context.Background(), wwnNum)
	if err != nil {
		log.Errorf("Failed to get block devices with error (%s)", err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}
	if len(deviceNames) > 0 {
		var devName string
		for _, deviceName := range deviceNames {
			devicePath := sysBlock + deviceName
			log.Infof("Rescanning unmounted (raw block) device %s to expand size", deviceName)
			err = s.Fs.GetUtil().DeviceRescan(context.Background(), devicePath)
			if err != nil {
				log.Errorf("Failed to rescan device (%s) with error (%s)", devicePath, err.Error())
				return nil, status.Error(codes.Internal, err.Error())
			}
			devName = deviceName
		}

		mpathDev, err := s.Fs.GetUtil().GetMpathNameFromDevice(ctx, devName)
		fmt.Println("mpathDev: " + mpathDev)
		if err != nil {
			log.Errorf("Failed to get mpath name for device (%s) with error (%s)", devName, err.Error())
			return nil, status.Error(codes.Internal, err.Error())
		}
		if mpathDev != "" {
			err = s.Fs.GetUtil().ResizeMultipath(context.Background(), mpathDev)
			if err != nil {
				log.Errorf("Failed to resize multipath of block device (%s) with error (%s)", mpathDev, err.Error())
				return nil, status.Error(codes.Internal, err.Error())
			}
		}

		log.Info("Block volume successfuly rescaned.")
		return &csi.NodeExpandVolumeResponse{}, nil
	}
	log.Error("No raw block devices found")
	return nil, status.Error(codes.NotFound, "No raw block devices found")
}

// NodeGetCapabilities returns supported features by the node service
func (s *Service) NodeGetCapabilities(context context.Context, request *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
				},
			},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_VOLUME_CONDITION,
					},
				},
			},
		},
	}, nil
}

// NodeGetInfo returns id of the node and topology constraints
func (s *Service) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	// Create the topology keys
	// <driver name>/<endpoint>-<protocol>: true
	resp := &csi.NodeGetInfoResponse{
		NodeId: s.nodeID,
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{},
		},
	}

	for _, arr := range s.Arrays() {
		_, err := getOutboundIP(arr.GetIP(), s.Fs)
		if err == nil {
			resp.AccessibleTopology.Segments[common.Name+"/"+arr.GetIP()+"-nfs"] = "true"
		}

		if arr.BlockProtocol != common.NoneTransport {
			if s.useFC {
				// Check node initiators connection to array
				nodeID := s.nodeID
				if s.reusedHost {
					ipList := common.GetIPListFromString(nodeID)
					if ipList == nil || len(ipList) == 0 {
						log.Errorf("can't find ip in nodeID %s", nodeID)
						continue
					}
					ip := ipList[len(ipList)-1]
					nodeID = nodeID[:len(nodeID)-len(ip)-1]
				}

				host, err := arr.GetClient().GetHostByName(ctx, nodeID)
				if err != nil {
					log.WithFields(log.Fields{
						"hostName": nodeID,
						"error":    err,
					}).Error("could not find host on PowerStore array")
					continue
				}

				if len(host.Initiators) == 0 {
					log.Error("host initiators array is empty")
					continue
				}

				if len(host.Initiators[0].ActiveSessions) != 0 {
					resp.AccessibleTopology.Segments[common.Name+"/"+arr.GetIP()+"-fc"] = "true"
				} else {
					log.WithFields(log.Fields{
						"hostName":  host.Name,
						"initiator": host.Initiators[0].PortName,
					}).Error("there is no active FC sessions")
					continue
				}
			} else {
				infoList, err := common.GetISCSITargetsInfoFromStorage(arr.GetClient())
				if err != nil {
					log.Errorf("couldn't get targets from array: %s", err.Error())
					continue
				}

				_, err = s.iscsiLib.DiscoverTargets(infoList[0].Portal, false)
				if err != nil {
					log.Error("couldn't discover targets")
					continue
				}

				resp.AccessibleTopology.Segments[common.Name+"/"+arr.GetIP()+"-iscsi"] = "true"
			}
		}
	}

	return resp, nil
}

func (s *Service) updateNodeID() error {
	if s.nodeID == "" {
		hostID, err := s.Fs.ReadFile(s.opts.NodeIDFilePath)
		if err != nil {
			log.WithFields(log.Fields{
				"path":  s.opts.NodeIDFilePath,
				"error": err,
			}).Error("Could not read Node ID file")
			return status.Errorf(codes.FailedPrecondition, "Could not readNode ID file: %s", err.Error())
		}

		// Check connection to array and get ip
		ip, err := getOutboundIP(s.DefaultArray().GetIP(), s.Fs)
		if err != nil {
			log.WithFields(log.Fields{
				"endpoint": s.DefaultArray().GetIP(),
				"error":    err,
			}).Error("Could not connect to PowerStore array")
			return status.Errorf(codes.FailedPrecondition, "Could not connect to PowerStore array: %s", err.Error())
		}

		nodeID := fmt.Sprintf(
			"%s-%s-%s", s.opts.NodeNamePrefix, strings.TrimSpace(string(hostID)), ip.String(),
		)

		if len(nodeID) > powerStoreMaxNodeNameLength {
			err := errors.New("node name prefix is too long")
			log.WithFields(log.Fields{
				"value": s.opts.NodeNamePrefix,
				"error": err,
			}).Error("Invalid Node ID")
			return err
		}
		s.nodeID = nodeID
	}
	return nil
}

func (s *Service) getInitiators() ([]string, []string, error) {
	ctx := context.Background()

	var iscsiAvailable bool
	var fcAvailable bool

	iscsiInitiators, err := s.iscsiConnector.GetInitiatorName(ctx)
	if err != nil {
		log.Error("nodeStartup could not GetInitiatorIQNs")
	} else if len(iscsiInitiators) == 0 {
		log.Error("iscsi initiators not found on node")
	} else {
		log.Debug("iscsi initiators found on node")
		iscsiAvailable = true
	}

	fcInitiators, err := s.getNodeFCPorts(ctx)
	if err != nil {
		log.Error("nodeStartup could not FC initiators for node")
	} else if len(fcInitiators) == 0 {
		log.Error("FC was not found or filtered with FCPortsFilterFile")
	} else {
		log.Error("FC initiators found on node")
		fcAvailable = true
	}

	if !iscsiAvailable && !fcAvailable {
		// If we haven't found any initiators we still can use NFS
		log.Info("FC and iSCSI initiators not found on node")
	}

	return iscsiInitiators, fcInitiators, nil
}

func (s *Service) getNodeFCPorts(ctx context.Context) ([]string, error) {
	var err error
	var initiators []string

	defer func() {
		initiators := initiators
		log.Infof("FC initiators found: %s", initiators)
	}()

	rawInitiatorsData, err := s.fcConnector.GetInitiatorPorts(ctx)
	if err != nil {
		log.Error("failed FC initiators list from node")
		return nil, err
	}

	for _, initiator := range rawInitiatorsData {
		data, err := formatWWPN(strings.TrimPrefix(initiator, "0x"))
		if err != nil {
			return nil, err
		}
		initiators = append(initiators, data)
	}
	if len(initiators) == 0 {
		return initiators, nil
	}
	portsFilter, _ := s.readFCPortsFilterFile()
	if len(portsFilter) == 0 {
		return initiators, nil
	}
	var filteredInitiators []string
	for _, filterValue := range portsFilter {
		for _, initiator := range initiators {
			if initiator != filterValue {
				continue
			}
			log.Infof("FC initiator port %s match filter", initiator)
			filteredInitiators = append(filteredInitiators, initiator)
		}
	}
	initiators = filteredInitiators

	return initiators, nil
}

func (s *Service) readFCPortsFilterFile() ([]string, error) {
	if s.opts.FCPortsFilterFilePath == "" {
		return nil, nil
	}
	data, err := s.Fs.ReadFile(s.opts.FCPortsFilterFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var result []string
	wwpns := strings.Split(strings.TrimSpace(string(data)), ",")
	for _, p := range wwpns {
		if !strings.Contains(p, ":") {
			log.Error("invalid FCPortsFilterFile format")
			return nil, nil
		}
		result = append(result, p)
	}
	return result, nil
}

func (s *Service) setupHost(initiators []string, client gopowerstore.Client, arrayIP string) error {
	log.Infof("setting up host on %s", arrayIP)
	defer log.Infof("finished setting up host on %s", arrayIP)

	if s.nodeID == "" {
		return fmt.Errorf("nodeID not set")
	}

	reqInitiators := s.buildInitiatorsArray(initiators)
	var host *gopowerstore.Host
	updateCHAP := false

	h, err := client.GetHostByName(context.Background(), s.nodeID)
	if err == nil {
		err := s.updateHost(context.Background(), initiators, client, h)
		if err != nil {
			return err
		}

		if s.opts.EnableCHAP && len(h.Initiators) > 0 && h.Initiators[0].ChapSingleUsername == "" {
			err := s.modifyHostInitiators(context.Background(), h.ID, client, nil, nil, initiators)
			if err != nil {
				return fmt.Errorf("can't modify initiators CHAP credentials %s", err.Error())
			}
		}

		s.initialized = true
		return nil
	}

	hosts, err := client.GetHosts(context.Background())
	if err != nil {
		log.Error(err.Error())
		return err
	}

	for i, h := range hosts {
		found := false
		for _, hI := range h.Initiators {
			for _, rI := range reqInitiators {
				if hI.PortName == *rI.PortName && hI.PortType == *rI.PortType {
					log.Info("Found existing host ", h.Name, hI.PortName, hI.PortType)
					updateCHAP = s.opts.EnableCHAP && hI.ChapSingleUsername == ""
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if found {
			host = &hosts[i]
			break
		}
	}

	if host == nil {
		// register node on PowerStore
		_, err := s.createHost(context.Background(), initiators, client)
		if err != nil {
			log.Error(err.Error())
			return err
		}
	} else {
		// node already registered
		if updateCHAP { // add CHAP credentials if they aren't available
			err := s.modifyHostInitiators(context.Background(), host.ID, client, nil, nil, initiators)
			if err != nil {
				return fmt.Errorf("can't modify initiators CHAP credentials %s", err.Error())
			}
		}

		ip, err := getOutboundIP(arrayIP, s.Fs)
		if err != nil {
			log.WithFields(log.Fields{
				"endpoint": arrayIP,
				"error":    err,
			}).Error("Could not connect to PowerStore array")
			return status.Errorf(codes.FailedPrecondition, "couldn't connect to PowerStore array: %s", err.Error())
		}

		s.nodeID = host.Name + "-" + ip.String()
		s.reusedHost = true
	}

	s.initialized = true

	return nil
}

func (s *Service) buildInitiatorsArray(initiators []string) []gopowerstore.InitiatorCreateModify {
	var portType gopowerstore.InitiatorProtocolTypeEnum
	if s.useFC {
		portType = gopowerstore.InitiatorProtocolTypeEnumFC
	} else {
		portType = gopowerstore.InitiatorProtocolTypeEnumISCSI
	}
	initiatorsReq := make([]gopowerstore.InitiatorCreateModify, len(initiators))
	for i, iqn := range initiators {
		iqn := iqn
		if !s.useFC && s.opts.EnableCHAP {
			initiatorsReq[i] = gopowerstore.InitiatorCreateModify{
				ChapSinglePassword: &s.opts.CHAPPassword,
				ChapSingleUsername: &s.opts.CHAPUsername,
				PortName:           &iqn,
				PortType:           &portType,
			}
		} else {
			initiatorsReq[i] = gopowerstore.InitiatorCreateModify{
				PortName: &iqn,
				PortType: &portType,
			}
		}
	}
	return initiatorsReq
}

// create or update host on PowerStore array
func (s *Service) updateHost(ctx context.Context, initiators []string, client gopowerstore.Client, host gopowerstore.Host) (err error) {
	initiatorsToAdd, initiatorsToDelete := checkIQNS(initiators, host)
	return s.modifyHostInitiators(ctx, host.ID, client, initiatorsToAdd, initiatorsToDelete, nil)
}

// register host
func (s *Service) createHost(ctx context.Context, initiators []string, client gopowerstore.Client) (id string, err error) {
	osType := gopowerstore.OSTypeEnumLinux
	reqInitiators := s.buildInitiatorsArray(initiators)
	description := fmt.Sprintf("k8s node: %s", s.opts.KubeNodeName)
	createParams := gopowerstore.HostCreate{Name: &s.nodeID, OsType: &osType, Initiators: &reqInitiators,
		Description: &description}
	resp, err := client.CreateHost(ctx, &createParams)
	if err != nil {
		return id, err
	}
	return resp.ID, err
}

// add or remove initiators from host
func (s *Service) modifyHostInitiators(ctx context.Context, hostID string, client gopowerstore.Client,
	initiatorsToAdd []string, initiatorsToDelete []string, initiatorsToModify []string) error {
	if len(initiatorsToDelete) > 0 {
		modifyParams := gopowerstore.HostModify{}
		modifyParams.RemoveInitiators = &initiatorsToDelete
		_, err := client.ModifyHost(ctx, &modifyParams, hostID)
		if err != nil {
			return err
		}
	}
	if len(initiatorsToAdd) > 0 {
		modifyParams := gopowerstore.HostModify{}
		initiators := s.buildInitiatorsArray(initiatorsToAdd)
		modifyParams.AddInitiators = &initiators
		_, err := client.ModifyHost(ctx, &modifyParams, hostID)
		if err != nil {
			return err
		}
	}
	if len(initiatorsToModify) > 0 {
		modifyParams := gopowerstore.HostModify{}
		initiators := s.buildInitiatorsArrayModify(initiatorsToModify)
		modifyParams.ModifyInitiators = &initiators
		_, err := client.ModifyHost(ctx, &modifyParams, hostID)
		if err != nil {
			return err
		}
	}
	return nil
}

func checkIQNS(IQNs []string, host gopowerstore.Host) (iqnToAdd, iqnToDelete []string) {
	// create map with initiators which are already exist
	initiatorMap := make(map[string]bool)
	for _, initiator := range host.Initiators {
		initiatorMap[initiator.PortName] = false
	}

	for _, iqn := range IQNs {
		_, ok := initiatorMap[iqn]
		if ok {
			// the iqn should be left in the host
			initiatorMap[iqn] = true
		} else {
			// the iqn should be added to the host
			iqnToAdd = append(iqnToAdd, iqn)
		}
	}

	// find iqns to delete from host
	for iqn, found := range initiatorMap {
		if !found {
			iqnToDelete = append(iqnToDelete, iqn)
		}
	}
	return
}

func (s *Service) buildInitiatorsArrayModify(initiators []string) []gopowerstore.UpdateInitiatorInHost {
	initiatorsReq := make([]gopowerstore.UpdateInitiatorInHost, len(initiators))
	for i, iqn := range initiators {
		iqn := iqn
		if !s.useFC && s.opts.EnableCHAP {
			initiatorsReq[i] = gopowerstore.UpdateInitiatorInHost{
				ChapSinglePassword: &s.opts.CHAPPassword,
				ChapSingleUsername: &s.opts.CHAPUsername,
				PortName:           &iqn,
			}
		} else {
			initiatorsReq[i] = gopowerstore.UpdateInitiatorInHost{
				PortName: &iqn,
			}
		}
	}
	return initiatorsReq
}

func (s *Service) fileExists(filename string) bool {
	_, err := s.Fs.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return true
}
