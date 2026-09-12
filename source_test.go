package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	pb "github.com/Tencent/WeKnora/pkg/plugin/proto/v1"
	"github.com/Tencent/WeKnora/pkg/plugin/sdk"
	"github.com/Tencent/WeKnora/pkg/plugin/sdk/contracttest"
	"golang.org/x/sys/unix"
)

type recording struct {
	up         []*pb.DocumentUpsert
	del        []string
	cursor     []byte
	itemErrors int
}

func (r *recording) send(e *pb.SyncEvent) error {
	if u := e.GetUpsert(); u != nil {
		r.up = append(r.up, u)
	}
	if d := e.GetDelete(); d != nil {
		r.del = append(r.del, d.ExternalId)
	}
	if c := e.GetCheckpoint(); c != nil {
		r.cursor = bytes.Clone(c.CursorJson)
	}
	if e.GetItemError() != nil {
		r.itemErrors++
	}
	return nil
}
func fixture(t *testing.T) (*source, string) {
	t.Helper()
	root := t.TempDir()
	s, err := newSource(root)
	must(t, err)
	t.Cleanup(func() { s.root.Close() })
	return s, root
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func put(t *testing.T, root, name, body string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755))
	must(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0644))
}
func scan(t *testing.T, s *source, cursor []byte, up, del int) recording {
	t.Helper()
	var r recording
	must(t, s.sync(context.Background(), &pb.SyncRequest{CursorJson: cursor}, r.send))
	if len(r.up) != up || len(r.del) != del || len(r.cursor) == 0 {
		t.Fatalf("up=%d delete=%d checkpoint=%t; want %d/%d/true", len(r.up), len(r.del), len(r.cursor) > 0, up, del)
	}
	t.Logf("up=%d delete=%d checkpoint=1", len(r.up), len(r.del))
	return r
}
func TestDiffReplayAndHistory(t *testing.T) {
	s, root := fixture(t)
	put(t, root, "a.txt", "A")
	put(t, root, "nested/b.txt", "B")
	put(t, root, "c.pdf", "%PDF-1.4\n\x00\xffraw")
	first := scan(t, s, nil, 3, 0)
	if first.up[0].ExternalId != "a.txt" || first.up[1].ExternalId != "c.pdf" || !bytes.Equal(first.up[1].Content, []byte("%PDF-1.4\n\x00\xffraw")) {
		t.Fatal("ordering/raw content changed")
	}
	replay := scan(t, s, nil, 3, 0)
	if !reflect.DeepEqual(first, replay) {
		t.Fatal("first scan replay not stable")
	}
	quiet := scan(t, s, first.cursor, 0, 0)
	put(t, root, "a.txt", "B")
	changed := scan(t, s, quiet.cursor, 1, 0)
	replay = scan(t, s, quiet.cursor, 1, 0)
	if !reflect.DeepEqual(changed, replay) {
		t.Fatal("old cursor replay not stable")
	}
	put(t, root, "a.txt", "A")
	again := scan(t, s, changed.cursor, 1, 0)
	if again.up[0].Revision == first.up[0].Revision {
		t.Fatal("A -> B -> A reused historical revision")
	}
	put(t, root, "d.txt", "D")
	added := scan(t, s, again.cursor, 1, 0)
	must(t, os.Remove(filepath.Join(root, "d.txt")))
	deleted := scan(t, s, added.cursor, 0, 1)
	put(t, root, "d.txt", "D")
	rebuilt := scan(t, s, deleted.cursor, 1, 0)
	if rebuilt.up[0].Revision == added.up[0].Revision {
		t.Fatal("recreated path reused historical revision")
	}
	must(t, os.Rename(filepath.Join(root, "d.txt"), filepath.Join(root, "e.txt")))
	renamed := scan(t, s, rebuilt.cursor, 1, 1)
	if renamed.up[0].ExternalId != "e.txt" || renamed.del[0] != "d.txt" {
		t.Fatal("rename must be delete + add")
	}
	var forced recording
	must(t, s.sync(context.Background(), &pb.SyncRequest{CursorJson: renamed.cursor, ForceFull: true}, forced.send))
	if len(forced.up) != 4 || forced.up[0].Revision != again.up[0].Revision {
		t.Fatal("force_full lost prior revision state")
	}
	// Simulate the Host's map[string]interface{} Cursor round trip, then a new process state.
	var opaque map[string]any
	must(t, json.Unmarshal(renamed.cursor, &opaque))
	cursor, err := json.Marshal(opaque)
	must(t, err)
	restarted, err := newSource(root)
	must(t, err)
	defer restarted.root.Close()
	scan(t, restarted, cursor, 0, 0)
}

func TestScanFailuresNeverDeleteOrCheckpoint(t *testing.T) {
	for _, kind := range []string{"symlink-file", "symlink-directory", "fifo", "unreadable", "empty", "oversized", "unsafe-name"} {
		t.Run(kind, func(t *testing.T) {
			s, root := fixture(t)
			put(t, root, "old.txt", "old")
			first := scan(t, s, nil, 1, 0)
			must(t, os.Remove(filepath.Join(root, "old.txt")))
			switch kind {
			case "symlink-file":
				must(t, os.Symlink("/etc/passwd", filepath.Join(root, "bad")))
			case "symlink-directory":
				must(t, os.Symlink(t.TempDir(), filepath.Join(root, "bad")))
			case "fifo":
				must(t, unix.Mkfifo(filepath.Join(root, "bad"), 0600))
			case "unreadable":
				put(t, root, "bad", "secret")
				must(t, os.Chmod(filepath.Join(root, "bad"), 0))
				if os.Geteuid() == 0 {
					t.Skip("permission test requires ordinary user")
				}
			case "empty":
				put(t, root, "bad", "")
			case "oversized":
				f, err := os.Create(filepath.Join(root, "bad"))
				must(t, err)
				must(t, f.Truncate(sdk.MaxDocumentBytes+1))
				must(t, f.Close())
			case "unsafe-name":
				put(t, root, "bad\"name", "x")
			}
			var r recording
			err := s.sync(context.Background(), &pb.SyncRequest{CursorJson: first.cursor}, r.send)
			if err == nil || len(r.del) != 0 || len(r.cursor) != 0 || r.itemErrors != 1 {
				t.Fatalf("unsafe scan committed: %+v error=%v", r, err)
			}
		})
	}
}

func TestCancellationReadChangeAndSendFailure(t *testing.T) {
	s, root := fixture(t)
	put(t, root, "a", "A")
	put(t, root, "b", "B")
	files, _, err := s.enumerate(context.Background())
	must(t, err)
	put(t, root, "a", "changed")
	if _, err = s.readFile(context.Background(), "a", files["a"]); err == nil {
		t.Fatal("changed file accepted")
	}
	for _, kind := range []string{"cancel", "send", "directory-change"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var r recording
			err := s.sync(ctx, &pb.SyncRequest{}, func(e *pb.SyncEvent) error {
				r.send(e)
				if e.GetUpsert() != nil {
					switch kind {
					case "cancel":
						cancel()
					case "send":
						return errors.New("receiver refused event")
					case "directory-change":
						put(t, root, "new", "N")
					}
				}
				return nil
			})
			if err == nil || len(r.cursor) > 0 {
				t.Fatalf("%s produced checkpoint: %v", kind, err)
			}
		})
	}
}

func TestPathReplacementBetweenEnumerationAndOpen(t *testing.T) {
	s, root := fixture(t)
	put(t, root, "nested/file", "authorized")
	files, _, err := s.enumerate(context.Background())
	must(t, err)
	outside := t.TempDir()
	put(t, outside, "file", "outside sentinel")
	must(t, os.Rename(filepath.Join(root, "nested"), filepath.Join(root, "original")))
	must(t, os.Symlink(outside, filepath.Join(root, "nested")))
	data, err := s.readFile(context.Background(), "nested/file", files["nested/file"])
	if err == nil || len(data) != 0 {
		t.Fatal("path replacement escaped the pinned root")
	}
}

func TestConfigCursorAndScaleBoundaries(t *testing.T) {
	for _, c := range []string{`{"settings":{"path":"/etc"}}`, `{"resource_ids":["../"]}`, `{"resource_ids":["/data/source"]}`, `{"credentials":{"key":"secret"}}`, `{"type":"http"}`, `{"path":"/tmp"}`} {
		if _, err := validateConfig([]byte(c)); err == nil {
			t.Fatalf("accepted %s", c)
		}
	}
	for _, c := range []string{`{}`, `null`, `garbage`, strings.Repeat("x", sdk.MaxCursorBytes+1), `{"connector_cursor":{"local_directory":{"version":2,"round":"1","files":{}}}}`, `{"connector_cursor":{"local_directory":{"version":1,"round":"0","files":{}}}}`} {
		s, _ := fixture(t)
		var r recording
		if err := s.sync(context.Background(), &pb.SyncRequest{CursorJson: []byte(c), ForceFull: true}, r.send); err == nil || len(r.cursor) > 0 {
			t.Fatal("invalid cursor was accepted")
		}
	}
	s, root := fixture(t)
	for i := 0; i <= maxFiles; i++ {
		put(t, root, fmt.Sprintf("%04d", i), "x")
	}
	var r recording
	if err := s.sync(context.Background(), &pb.SyncRequest{}, r.send); err == nil || len(r.cursor) > 0 || len(r.up) > 0 {
		t.Fatal("over-limit scan accepted")
	}
	state := snapshot{Version: 1, Round: "1", Files: map[string]fileState{}}
	for i := 0; i < maxFiles; i++ {
		state.Files[fmt.Sprintf("%04d%s", i, strings.Repeat("<", maxPathBytes-4))] = fileState{Digest: strings.Repeat("a", 64), Revision: strings.Repeat("b", 64)}
	}
	if _, err := encodeCursor(state); err == nil {
		t.Fatal("over-limit encoded cursor accepted")
	}
	state.Files = map[string]fileState{"../escape": {Digest: strings.Repeat("a", 64), Revision: strings.Repeat("b", 64)}}
	encoded, err := encodeCursor(state)
	must(t, err)
	if _, _, err := readCursor(encoded); err == nil {
		t.Fatal("cursor path traversal accepted")
	}
}

func TestPublicSDKContract(t *testing.T) {
	s, root := fixture(t)
	put(t, root, "hello.pdf", "%PDF-1.4\n\x00raw")
	id := identity([]byte("contract-test-nonce"))
	control, err := sdk.NewControlServer(id, sdk.ControlHooks{ValidateConfig: func(_ context.Context, b []byte) error { _, err := validateConfig(b); return err }, Health: s.health})
	must(t, err)
	contracttest.Run(t, contracttest.Options{Identity: id, Services: sdk.Services{Control: control, DataSource: s}, ConfigJSON: []byte(`{"type":"local_directory","resource_ids":["grant_root"],"settings":{}}`), RequiredCapabilities: id.SupportedCapabilities,
		VerifyDataSource: func(ctx context.Context, c pb.DataSourcePluginClient) error {
			list, err := c.ListResources(ctx, &pb.ListResourcesRequest{})
			if err != nil {
				return err
			}
			if len(list.Resources) != 1 || list.Resources[0].ExternalId != rootID || list.Resources[0].HasChildren {
				return errors.New("invalid root resource")
			}
			children, err := c.ListResources(ctx, &pb.ListResourcesRequest{ParentId: rootID})
			if err != nil {
				return err
			}
			if len(children.Resources) > 0 {
				return errors.New("root exposed child resources")
			}
			if _, err = c.ListResources(ctx, &pb.ListResourcesRequest{ParentId: "unknown"}); err == nil {
				return errors.New("unknown parent accepted")
			}
			ancestors, err := c.ResolveAncestors(ctx, &pb.ResolveAncestorsRequest{ResourceIds: []string{rootID}})
			if err != nil {
				return err
			}
			if len(ancestors.AncestorIds) > 0 {
				return errors.New("root has ancestors")
			}
			stream, err := c.Sync(ctx, &pb.SyncRequest{})
			if err != nil {
				return err
			}
			var r recording
			for {
				e, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				r.send(e)
			}
			if len(r.up) != 1 || !bytes.Equal(r.up[0].Content, []byte("%PDF-1.4\n\x00raw")) || len(r.cursor) == 0 {
				return errors.New("UDS raw file/checkpoint mismatch")
			}
			return nil
		}})
}
