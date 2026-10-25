package update

import "testing"

func TestNewRetainedDatasetPreservesExistingGenerationVolumes(t *testing.T) {
	e := &Engine{Dep: &Deployment{Workspaces: map[string]WorkspaceConfig{"default": {Datasets: []Dataset{
		{Key: "profile", Role: RoleSnapshot},
		{Key: "policy", Role: RoleRetained},
		{Key: "missing-snapshot", Role: RoleSnapshot},
	}}}}}
	st := State{Components: map[string]ComponentState{"runtime/default": {Volumes: map[string]string{"profile": "profile-generation-12"}}}}
	model := ComposeModel{Volumes: map[string]ComposeVolume{"profile": {Name: "profile-default"}, "policy": {Name: "policy-stable"}, "missing-snapshot": {Name: "snapshot-default"}}}
	if err := e.includeRetainedDatasets(&st, model); err != nil {
		t.Fatal(err)
	}
	volumes := st.Components["runtime/default"].Volumes
	if volumes["profile"] != "profile-generation-12" || volumes["policy"] != "policy-stable" {
		t.Fatalf("existing snapshot remapped or retained dataset not enrolled: %+v", volumes)
	}
	if _, ok := volumes["missing-snapshot"]; ok {
		t.Fatal("new snapshot enrolled without a checkpoint")
	}
}

func TestNewRetainedDatasetRequiresResolvedVolume(t *testing.T) {
	e := &Engine{Dep: &Deployment{Workspaces: map[string]WorkspaceConfig{"default": {Datasets: []Dataset{{Key: "policy", Role: RoleRetained}}}}}}
	st := State{Components: map[string]ComponentState{"runtime/default": {}}}
	if err := e.includeRetainedDatasets(&st, ComposeModel{}); err == nil {
		t.Fatal("unresolved volume enrolled")
	}
}
