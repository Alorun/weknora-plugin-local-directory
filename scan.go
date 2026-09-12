package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"syscall"

	pb "github.com/Tencent/WeKnora/pkg/plugin/proto/v1"
	"github.com/Tencent/WeKnora/pkg/plugin/sdk"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (s *source) open(name string) (*os.File, error) {
	fd, err := unix.Openat2(int(s.root.Fd()), name, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
		f.Close()
		return nil, fmt.Errorf("not a readable ordinary file/directory: %q", name)
	}
	// O_PATH inspects the inode without opening a device/FIFO for I/O. Reopen
	// this pinned, verified inode, not its mutable pathname. /proc/self/fd is a
	// trusted kernel reference; no source-controlled symlink is followed.
	opened, err := os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", name, err)
	}
	return opened, nil
}

func same(a, b os.FileInfo) bool {
	x, okX := a.Sys().(*syscall.Stat_t)
	y, okY := b.Sys().(*syscall.Stat_t)
	return okX && okY && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime() == b.ModTime() && x.Ctim == y.Ctim
}

func (s *source) enumerate(ctx context.Context) (map[string]os.FileInfo, map[string]os.FileInfo, error) {
	files, dirs := map[string]os.FileInfo{}, map[string]os.FileInfo{}
	entries := 0
	var walk func(string, int) error
	walk = func(name string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > maxEntries || depth > maxDepth {
			return errors.New("directory entry/depth limit exceeded")
		}
		f, err := s.open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if !info.IsDir() {
			if len(files) >= maxFiles {
				return errors.New("file count limit exceeded")
			}
			files[name] = info
			return nil
		}
		dirs[name] = info
		for {
			names, err := f.Readdirnames(64)
			if err != nil && err != io.EOF {
				return err
			}
			for _, child := range names {
				rel := path.Join(name, child)
				if !validPath(rel) {
					return fmt.Errorf("unsafe or overlong filename: %q", rel)
				}
				if err := walk(rel, depth+1); err != nil {
					return err
				}
			}
			if err == io.EOF {
				break
			}
		}
		return nil
	}
	err := walk(".", 0)
	return files, dirs, err
}

type cancelReader struct {
	ctx context.Context
	io.Reader
}

func (r cancelReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(b)
}

func (s *source) readFile(ctx context.Context, name string, expected os.FileInfo) ([]byte, error) {
	f, err := s.open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || !same(expected, before) {
		return nil, errors.New("source changed before reading")
	}
	if before.Size() == 0 {
		return nil, errors.New("empty files are not accepted by current Host ingestion")
	}
	if before.Size() > sdk.MaxDocumentBytes {
		return nil, errors.New("file exceeds 32 MiB document limit")
	}
	data, err := io.ReadAll(io.LimitReader(cancelReader{ctx, f}, sdk.MaxDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !same(before, after) || int64(len(data)) != before.Size() {
		return nil, errors.New("source changed while reading")
	}
	if err := s.verify(name, before); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *source) verify(name string, expected os.FileInfo) error {
	f, err := s.open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !same(expected, info) {
		return fmt.Errorf("source changed during scan: %q", name)
	}
	return nil
}

func (s *source) sync(ctx context.Context, req *pb.SyncRequest, send func(*pb.SyncEvent) error) error {
	if _, err := validateConfig(req.GetConfigJson()); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	old, round, err := readCursor(req.GetCursorJson())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if round == math.MaxUint64 {
		return status.Error(codes.OutOfRange, "cursor round exhausted")
	}
	fail := func(id string, err error) error {
		if ctx.Err() != nil {
			return status.FromContextError(ctx.Err()).Err()
		}
		if sendErr := send(&pb.SyncEvent{Event: &pb.SyncEvent_ItemError{ItemError: &pb.ItemError{ExternalId: id, Code: sdk.ErrorUnavailable, SafeMessage: err.Error()}}}); sendErr != nil {
			return sendErr
		}
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	files, dirs, err := s.enumerate(ctx)
	if err != nil {
		return fail("", err)
	}
	next := snapshot{Version: 1, Round: strconv.FormatUint(round+1, 10), Files: map[string]fileState{}}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data, err := s.readFile(ctx, name, files[name])
		if err != nil {
			return fail(name, err)
		}
		h := sha256.Sum256(data)
		digest := hex.EncodeToString(h[:])
		state, exists := old.Files[name]
		changed := !exists || state.Digest != digest
		if changed {
			state = fileState{Digest: digest, Revision: revision(round+1, name, digest)}
		}
		next.Files[name] = state
		if changed || req.GetForceFull() {
			e := &pb.SyncEvent{Event: &pb.SyncEvent_Upsert{Upsert: &pb.DocumentUpsert{ExternalId: name, Revision: state.Revision, Title: name, Content: data,
				ContentType: http.DetectContentType(data), FileName: path.Base(name), SourceResourceId: rootID}}}
			if proto.Size(e) > sdk.MaxMessageBytes {
				return fail(name, errors.New("event exceeds SDK message limit"))
			}
			if err := ctx.Err(); err != nil {
				return status.FromContextError(err).Err()
			}
			if err := send(e); err != nil {
				return err
			}
		}
	}
	// No Delete until every file was read successfully and the enumerated
	// directories still match. This is not a whole-filesystem atomic snapshot.
	for name, info := range dirs {
		if err := s.verify(name, info); err != nil {
			return fail(name, err)
		}
	}
	cursor, err := encodeCursor(next)
	if err != nil {
		return fail("", err)
	}
	names = names[:0]
	for name := range old.Files {
		if _, exists := next.Files[name]; !exists {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		if err := send(&pb.SyncEvent{Event: &pb.SyncEvent_Delete{Delete: &pb.DocumentDelete{ExternalId: name, Title: name}}}); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return send(&pb.SyncEvent{Event: &pb.SyncEvent_Checkpoint{Checkpoint: &pb.Checkpoint{CursorJson: cursor}}})
}
