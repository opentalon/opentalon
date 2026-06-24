package main

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/orchestrator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeCapRefresher struct {
	names     []string
	caps      map[string]orchestrator.PluginCapability
	errs      map[string]error
	refreshed []string
}

func (f *fakeCapRefresher) List() []string { return f.names }

func (f *fakeCapRefresher) RefreshCapabilities(_ context.Context, name string) (orchestrator.PluginCapability, error) {
	f.refreshed = append(f.refreshed, name)
	if err := f.errs[name]; err != nil {
		return orchestrator.PluginCapability{}, err
	}
	return f.caps[name], nil
}

type fakeCapRegistry struct{ updated []string }

func (f *fakeCapRegistry) UpdateCapability(name string, _ orchestrator.PluginCapability) {
	f.updated = append(f.updated, name)
}

type fakeCorpusSyncer struct{ synced []string }

func (f *fakeCorpusSyncer) SyncPluginActions(_ context.Context, name string) {
	f.synced = append(f.synced, name)
}

type fakePluginLocker struct {
	acquire  map[string]bool
	released []string
}

func (f *fakePluginLocker) AcquireOrWait(context.Context) (bool, error) { return true, nil }
func (f *fakePluginLocker) ReleaseDone(context.Context)                 {}
func (f *fakePluginLocker) ReleaseAbort(context.Context)                {}
func (f *fakePluginLocker) TryAcquirePlugin(_ context.Context, name string) (bool, error) {
	return f.acquire[name], nil
}
func (f *fakePluginLocker) ReleasePlugin(_ context.Context, name string) {
	f.released = append(f.released, name)
}

// TestRefreshAllCapabilities covers the three poll outcomes in one cycle:
//   - mcp: refreshable AND this pod holds the lock → refreshed, registry
//     updated, corpus synced, lock released.
//   - weaviate: returns Unimplemented (not refreshable) → skipped entirely
//     (no registry update, no sync).
//   - api: refreshable but this pod is NOT the leader → refreshed + registry
//     updated (every pod keeps a fresh executable view), but corpus NOT synced
//     (another pod owns the write).
func TestRefreshAllCapabilities(t *testing.T) {
	ref := &fakeCapRefresher{
		names: []string{"mcp", "weaviate", "api"},
		caps: map[string]orchestrator.PluginCapability{
			"mcp": {Name: "mcp"},
			"api": {Name: "api"},
		},
		errs: map[string]error{
			"weaviate": status.Error(codes.Unimplemented, "no refresh"),
		},
	}
	reg := &fakeCapRegistry{}
	syncer := &fakeCorpusSyncer{}
	locker := &fakePluginLocker{acquire: map[string]bool{"mcp": true, "api": false}}

	refreshAllCapabilities(context.Background(), ref, reg, syncer, locker)

	if got := strings.Join(ref.refreshed, ","); got != "mcp,weaviate,api" {
		t.Errorf("refreshed = %q, want mcp,weaviate,api", got)
	}
	if got := strings.Join(reg.updated, ","); got != "mcp,api" {
		t.Errorf("registry updated = %q, want mcp,api (weaviate is Unimplemented)", got)
	}
	if got := strings.Join(syncer.synced, ","); got != "mcp" {
		t.Errorf("synced = %q, want mcp (only the leader-held plugin)", got)
	}
	if got := strings.Join(locker.released, ","); got != "mcp" {
		t.Errorf("released = %q, want mcp (release only where acquired)", got)
	}
}
