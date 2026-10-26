package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
)

type parityFixture struct {
	Models []struct {
		Picker  json.RawMessage
		IDs     []string
		Catalog []struct {
			ID, Name string
			Context  int
			Levels   map[string]string
			Fast     bool
		}
		Resolutions []struct {
			Model, Effort, Tier, ID string
			Max                     bool
			Params                  map[string]string
		}
	} `json:"modelCases"`
	Conversations []struct {
		Chat     chatRequest
		Messages []any
		Last     string
	}
	PB []struct {
		Value any
		Hex   string
	}
}

func referenceFixtures(t *testing.T) parityFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/reference-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var f parityFixture
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}
func TestReferenceModelCatalogParity(t *testing.T) {
	for n, c := range referenceFixtures(t).Models {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			ids := map[string]bool{}
			if c.IDs == nil {
				ids = nil
			} else {
				for _, id := range c.IDs {
					ids[id] = true
				}
			}
			entries, err := parseModels(append(append([]byte(`{"models":`), c.Picker...), '}'), ids)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(c.Catalog) {
				t.Fatalf("catalog size %d != %d", len(entries), len(c.Catalog))
			}
			for i, want := range c.Catalog {
				got := entries[i]
				if got.ID != want.ID || got.Name != want.Name || got.Context != want.Context || got.Fast != want.Fast || !reflect.DeepEqual(got.Levels, want.Levels) {
					t.Errorf("catalog %s differs: %+v vs %+v", want.ID, got, want)
				}
			}
		})
	}
}
func TestReferenceModelResolutionParity(t *testing.T) {
	for n, c := range referenceFixtures(t).Models {
		ids := map[string]bool{}
		if c.IDs == nil {
			ids = nil
		} else {
			for _, id := range c.IDs {
				ids[id] = true
			}
		}
		entries, err := parseModels(append(append([]byte(`{"models":`), c.Picker...), '}'), ids)
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range c.Resolutions {
			t.Run(fmt.Sprintf("%d/%d/%s/%s/%s", n, i, want.Model, want.Effort, want.Tier), func(t *testing.T) {
				got := resolveModel(entries, want.Model, want.Effort, want.Tier)
				params := map[string]string{}
				for _, p := range got.Params {
					params[p.ID] = p.Value
				}
				if got.ID != want.ID || got.Max != want.Max || !reflect.DeepEqual(params, want.Params) {
					t.Errorf("got %s max=%v %v; want %s max=%v %v", got.ID, got.Max, params, want.ID, want.Max, want.Params)
				}
			})
		}
	}
}
func TestReferenceConversationParity(t *testing.T) {
	for i, c := range referenceFixtures(t).Conversations {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			conv, err := conversation(c.Chat, toolsOf(c.Chat))
			if err != nil {
				t.Fatal(err)
			}
			messages := []any{}
			for _, raw := range conv.Messages {
				var msg any
				if err = json.Unmarshal(raw, &msg); err != nil {
					t.Fatal(err)
				}
				messages = append(messages, msg)
			}
			if conv.Last != c.Last || !reflect.DeepEqual(messages, c.Messages) {
				t.Errorf("conversation differs\ngot: %s\nwant: %s", encodeJSON(messages), encodeJSON(c.Messages))
			}
		})
	}
}
func TestReferenceProtobufValueParity(t *testing.T) {
	for i, c := range referenceFixtures(t).PB {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			if got := hex.EncodeToString(pbValue(c.Value)); got != c.Hex {
				t.Errorf("wire value %s != %s", got, c.Hex)
			}
			raw, _ := hex.DecodeString(c.Hex)
			got, err := pbAny(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.Value) {
				t.Errorf("decoded %v != %v", got, c.Value)
			}
		})
	}
}
