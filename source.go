package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"

	pb "github.com/Tencent/WeKnora/pkg/plugin/proto/v1"
	"github.com/Tencent/WeKnora/pkg/plugin/sdk"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type source struct {
	pb.UnimplementedDataSourcePluginServer
	root   *os.File
	syncMu sync.Mutex
}

// rootPath is supplied only by main's fixed path, or by same-package tests.
// All descendant opens are kernel-constrained relative to this descriptor.
func newSource(rootPath string) (*source, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, rootPath, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, errors.New("cannot open authorized root (Linux openat2 required)")
	}
	return &source{root: os.NewFile(uintptr(fd), "grant_root")}, nil
}

type config struct {
	Type        string                     `json:"type"`
	Credentials map[string]json.RawMessage `json:"credentials"`
	ResourceIDs []string                   `json:"resource_ids"`
	Settings    map[string]json.RawMessage `json:"settings"`
}

func decodeStrict(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func validateConfig(data []byte) (config, error) {
	var c config
	if len(data) > sdk.MaxConfigBytes {
		return c, errors.New("configuration exceeds SDK limit")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte(`{}`)
	}
	if err := decodeStrict(data, &c); err != nil {
		return c, errors.New("invalid datasource configuration")
	}
	if c.Type != "" && c.Type != extensionID || len(c.Credentials) != 0 || len(c.Settings) != 0 {
		return c, errors.New("only local_directory, empty settings and no credentials are accepted")
	}
	return c, validateSelection(c.ResourceIDs)
}

func validateSelection(ids []string) error {
	for _, id := range ids {
		if id != rootID {
			return errors.New("unknown resource: only grant_root is selectable")
		}
	}
	return nil
}

func (s *source) health(ctx context.Context) (pb.HealthStatus, error) {
	if err := ctx.Err(); err != nil {
		return pb.HealthStatus_HEALTH_STATUS_NOT_READY, err
	}
	f, err := s.open(".")
	if err != nil {
		return pb.HealthStatus_HEALTH_STATUS_NOT_READY, errors.New("authorized root is unavailable")
	}
	defer f.Close()
	return pb.HealthStatus_HEALTH_STATUS_READY, nil
}

func (s *source) ListResources(ctx context.Context, req *pb.ListResourcesRequest) (*pb.ListResourcesResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if _, err := validateConfig(req.GetConfigJson()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	switch req.GetParentId() {
	case "":
		return &pb.ListResourcesResponse{Resources: []*pb.Resource{{ExternalId: rootID, Name: "已授权目录", Type: "directory", HasChildren: false}}}, nil
	case rootID:
		return &pb.ListResourcesResponse{}, nil
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown parent resource")
	}
}

func (s *source) ResolveAncestors(ctx context.Context, req *pb.ResolveAncestorsRequest) (*pb.ResolveAncestorsResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if _, err := validateConfig(req.GetConfigJson()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := validateSelection(req.GetResourceIds()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.ResolveAncestorsResponse{}, nil
}

func (s *source) Sync(req *pb.SyncRequest, stream grpc.ServerStreamingServer[pb.SyncEvent]) error {
	if !s.syncMu.TryLock() {
		return status.Error(codes.ResourceExhausted, "sync already in progress")
	}
	defer s.syncMu.Unlock()
	return s.sync(stream.Context(), req, stream.Send)
}
