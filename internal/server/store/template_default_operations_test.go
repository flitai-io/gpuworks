package store

import "testing"

func TestIsDefaultTemplateOperations(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want bool
	}{
		"legacy chat":       {`[{"type":"openai.chat"}]`, true},
		"registered chat":   {`[{"inputs":null,"type":"openai.chat"}]`, true},
		"bare video":        {`[{"type":"openai.video.generate"}]`, true},
		"registered video":  {`[{"inputs":null,"type":"openai.video.generate"}]`, true},
		"reordered keys":    {`[ { "type": "openai.video.generate", "inputs": null } ]`, true},
		"explicit JSON":     {`[{"type":"openai.video.generate","endpoint":"/v1/videos/sync","mode":"sync","request_encoding":"json"}]`, false},
		"explicit encoding": {`[{"type":"openai.video.generate","request_encoding":"json"}]`, false},
		"explicit endpoint": {`[{"type":"openai.video.generate","endpoint":"/v1/videos"}]`, false},
		"explicit mode":     {`[{"type":"openai.video.generate","mode":"async"}]`, false},
		"explicit inputs":   {`[{"type":"openai.video.generate","inputs":[]}]`, false},
		"explicit defaults": {`[{"type":"openai.video.generate","request_defaults":{"seconds":1}}]`, false},
		"registered image":  {`[{"inputs":null,"type":"openai.image.generate"}]`, true},
		"bare embeddings":   {`[{"type":"openai.embeddings"}]`, true},
		"multiple bare":     {`[{"inputs":null,"type":"openai.image.generate"},{"inputs":null,"type":"openai.image.edit"}]`, true},
		"multiple, one set": {`[{"type":"openai.image.generate"},{"type":"openai.image.edit","endpoint":"/v1/images/edits"}]`, false},
		"unknown operation": {`[{"type":"openai.responses"}]`, false},
		"realtime":          {`[{"type":"openai.realtime"}]`, false},
		"missing type":      {`[{"inputs":null}]`, false},
		"null operation":    {`[null]`, false},
		"non-string type":   {`[{"type":1}]`, false},
		"empty":             {`[]`, false},
		"null":              {`null`, false},
		"malformed":         {`not-json`, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := IsDefaultTemplateOperations(tc.raw); got != tc.want {
				t.Fatalf("IsDefaultTemplateOperations(%s) = %t, want %t", tc.raw, got, tc.want)
			}
		})
	}
	if IsDefaultChatOperations(`[{"type":"openai.video.generate"}]`) {
		t.Fatal("the historical chat-only migration predicate must remain unchanged")
	}
}
