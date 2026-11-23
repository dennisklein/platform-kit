package schema_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dennisklein/platform-kit/internal/kinds/all"
	"github.com/dennisklein/platform-kit/internal/schema"
)

// TestDumpAllSchemas generates the schema of every kind version. With
// PK_DUMP_SCHEMAS=<dir> it writes them to files for inspection.
func TestDumpAllSchemas(t *testing.T) {
	dump := os.Getenv("PK_DUMP_SCHEMAS")
	reg := all.Registry()
	if len(reg.List()) == 0 {
		t.Fatal("empty registry")
	}
	for _, k := range reg.List() {
		if k.Schema == nil {
			t.Errorf("%s: no schema", k.Name)
			continue
		}
		for _, v := range k.Versions {
			b, err := k.Schema(v)
			if err != nil {
				t.Errorf("%s %s: %v", k.Name, v, err)
				continue
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Errorf("%s %s: invalid JSON: %v", k.Name, v, err)
				continue
			}
			if m["type"] != "object" {
				t.Errorf("%s %s: root type %v", k.Name, v, m["type"])
			}
			if dump != "" {
				name := strings.ToLower(k.Name) + "-" + v + ".json"
				if err := os.WriteFile(filepath.Join(dump, name), b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func TestExtensions(t *testing.T) {
	k, ok := all.Registry().Get("Node")
	if !ok {
		t.Fatal("no Node kind")
	}
	b, err := k.Schema("")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Kind       map[string]string `json:"x-pk-kind"`
		Properties struct {
			Spec struct {
				Properties map[string]map[string]any `json:"properties"`
			} `json:"spec"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.Kind["apiVersion"] != "inventory.pk.io/v1beta1" {
		t.Errorf("x-pk-kind: %v", s.Kind)
	}
	p := s.Properties.Spec.Properties
	if p["serial"][schema.XOwner] != "asset-db" || p["serial"][schema.XTruth] != "asserted" {
		t.Errorf("serial: %v", p["serial"])
	}
	if ref, _ := p["cluster"][schema.XRef].(map[string]any); ref["kind"] != "Cluster" {
		t.Errorf("cluster ref: %v", p["cluster"])
	}
	if ui, _ := p["location"][schema.XUI].(map[string]any); ui["widget"] != "rack-picker" {
		t.Errorf("location ui: %v", p["location"])
	}
	bmc, _ := p["bmc"]["properties"].(map[string]any)
	fixture, _ := bmc["fixture"].(map[string]any)
	if fixture[schema.XTruth] != "derived" {
		t.Errorf("bmc.fixture truth: %v", fixture)
	}
}

func TestParseTag(t *testing.T) {
	tag, err := schema.ParseTag("truth=intent,secret,ref=Node,widget=select,order=3")
	if err != nil || !tag.Secret || tag.Ref != "Node" || tag.UI["order"] != 3 {
		t.Fatalf("got %+v %v", tag, err)
	}
	if _, err := schema.ParseTag("truth=maybe"); err == nil {
		t.Fatal("want error for unknown truth class")
	}
}
