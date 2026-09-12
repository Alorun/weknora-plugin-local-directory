package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/Tencent/WeKnora/pkg/plugin/proto/v1"
	"github.com/Tencent/WeKnora/pkg/plugin/sdk"
)

const pluginID = "community.local-directory"
const pluginVersion = "0.1.0"
const extensionID = "local_directory"
const rootID = "grant_root"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "local-directory:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 1 || os.Getenv("WEKNORA_PLUGIN_BOOTSTRAP") != "/run/weknora/bootstrap.json" {
		return errors.New("managed C1 bootstrap required; no user paths or arguments accepted")
	}
	f, err := os.Open("/run/weknora/bootstrap.json")
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	f.Close()
	var boot struct{ StartupNonce []byte }
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &boot) != nil || len(boot.StartupNonce) == 0 || len(boot.StartupNonce) > 256 {
		return errors.New("invalid managed bootstrap")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	s, err := newSource("/data/source")
	if err != nil {
		return err
	}
	defer s.root.Close()
	control, err := sdk.NewControlServer(identity(boot.StartupNonce), sdk.ControlHooks{
		ValidateConfig: func(ctx context.Context, data []byte) error { _, err := validateConfig(data); return err },
		Health:         s.health,
		Shutdown: func(context.Context, time.Duration) error {
			// Let the SDK send its response before stopping the listener.
			time.AfterFunc(20*time.Millisecond, cancel)
			return nil
		},
	})
	if err != nil {
		return err
	}
	server, err := sdk.ServeUDS(ctx, "/run/weknora/plugin.sock", sdk.Services{Control: control, DataSource: s})
	if err != nil {
		return err
	}
	return <-server.Done()
}

func identity(nonce []byte) sdk.Identity {
	return sdk.Identity{PluginID: pluginID, PluginVersion: pluginVersion, ExtensionID: extensionID,
		ExtensionType: pb.ExtensionType_EXTENSION_TYPE_DATASOURCE, ProtocolVersion: sdk.ProtocolVersion,
		ContractVersion: sdk.DataSourceContractVersion, StartupNonce: nonce,
		SupportedCapabilities: []string{"resource_listing", "full_sync", "incremental_sync", "deletion_events"}}
}
