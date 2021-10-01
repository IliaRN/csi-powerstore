package controller

import (
	"context"
	volumeGroupSnapshot "github.com/dell/dell-csi-extensions/volumeGroupSnapshot"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) CreateVolumeGroupSnapshot(ctx context.Context, req *volumeGroupSnapshot.CreateVolumeGroupSnapshotRequest) (
	*volumeGroupSnapshot.CreateVolumeGroupSnapshotResponse, error) {
	log.Infof("CreateVolumeGroupSnapshot called with req: %v", req)

	err := validateCreateVGSreq(req)
	if err != nil {
		log.Errorf("Error from CreateVolumeGroupSnapshot: %v ", err)
		return nil, err
	}
	log.Infof("VGSnapshot Request--->%v", req)

	return nil, nil
}

//This function is a no-op for the driver, controller will decide what to do given the retain policy
func (s *Service) DeleteVolumeGroupSnapshot(ctx context.Context, req *volumeGroupSnapshot.DeleteVolumeGroupSnapshotRequest) (*volumeGroupSnapshot.DeleteVolumeGroupSnapshotResponse, error) {
	log.Infof("DeleteVolumeGroupSnapshot called %+v", req)
	return &volumeGroupSnapshot.DeleteVolumeGroupSnapshotResponse{}, nil
}

//validate if request has source volumes, a VGS name, and VGS name length < 27 chars
func validateCreateVGSreq(req *volumeGroupSnapshot.CreateVolumeGroupSnapshotRequest) error {
	if len(req.SourceVolumeIDs) == 0 {
		err := status.Errorf(codes.InvalidArgument, "SourceVolumeIDs cannot be empty")
		log.Errorf("Error from validateCreateVGSreq: %v ", err)
		return err
	}

	if req.Name == "" {
		err := status.Error(codes.InvalidArgument, "CreateVolumeGroupSnapshotRequest needs Name to be set")
		log.Errorf("Error from validateCreateVGSreq: %v ", err)
		return err
	}

	//name must be less than 28 chars, because we name snapshots with -<index>, and index can at most be 3 chars
	if len(req.Name) > 27 {
		err := status.Errorf(codes.InvalidArgument, "Requested name %s longer than 27 character max", req.Name)
		log.Errorf("Error from validateCreateVGSreq: %v ", err)
		return err
	}

	return nil
}
