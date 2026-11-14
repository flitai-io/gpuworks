package inferenceapi

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestDiscoveryAndPrimaryInputsHaveExactProtocolDefinitions(t *testing.T) {
	inputs := [][]string{{"messages"}, {"prompt"}, {"input"}, {"query", "documents"}, {"file"}, {"input"}, {"prompt"}, {"prompt", "image"}, {"prompt"}, {}, {"state", "questions"}}
	for index, def := range Definitions() {
		want := []DiscoveryRoute{{"models", "/v1/models"}, {"openapi", "/openapi.json"}}
		if def.Type == OperationAudioSpeech {
			want = append(want, DiscoveryRoute{"voices", "/v1/audio/voices"})
		}
		if !reflect.DeepEqual(def.PrimaryInputs, inputs[index]) || !reflect.DeepEqual(def.Discovery, want) {
			t.Fatalf("%s inputs=%v discovery=%v", def.Type, def.PrimaryInputs, def.Discovery)
		}
		wire, err := json.Marshal(def)
		var roundTrip Definition
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(wire, &roundTrip); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(roundTrip, def) {
			t.Fatalf("round trip=%s", wire)
		}
		def.PrimaryInputs = append(def.PrimaryInputs, "mutated")
		def.Discovery[0].Endpoint = "/mutated"
		fresh, _ := Lookup(def.Type)
		if !reflect.DeepEqual(fresh.PrimaryInputs, inputs[index]) || !reflect.DeepEqual(fresh.Discovery, want) {
			t.Fatal("caller mutated registry")
		}
	}
}

func TestDiscoveryOnlyAllowsRegisteredReadOnlyRoutes(t *testing.T) {
	for _, definition := range Definitions() {
		op := Operation{Type: definition.Type}
		for _, path := range []string{"/v1/models", "/openapi.json", "/v1/audio/voices", "/v1/audio/voices/x", "/openapi.json/secret"} {
			for _, method := range []string{"GET", "POST", "DELETE", "PUT", "PATCH"} {
				want := method == "GET" && (path == "/v1/models" || path == "/openapi.json" || path == "/v1/audio/voices" && definition.Type == OperationAudioSpeech)
				if got := AllowsRequest(op, method, path); got != want {
					t.Fatalf("%s %s %s allowed=%v want=%v", definition.Type, method, path, got, want)
				}
			}
		}
	}
}
