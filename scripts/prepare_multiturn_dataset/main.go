// Command prepare_multiturn_dataset creates a size-bounded, deterministic
// OpenAI-messages subset from OpenAssistant OASST1 ready conversation trees.
// It uses only the standard library so the checked-in gzip can be reproduced
// without adding build dependencies.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"
)

const (
	oasstRevision = "fdf72ae0827c1cda404aff25b6603abec9e3399b"
	defaultSource = "https://huggingface.co/datasets/OpenAssistant/oasst1/resolve/" + oasstRevision + "/2023-04-12_oasst_ready.trees.jsonl.gz"
)

type upstreamNode struct {
	Text      string         `json:"text"`
	Role      string         `json:"role"`
	Rank      *int           `json:"rank"`
	Deleted   bool           `json:"deleted"`
	Synthetic bool           `json:"synthetic"`
	ReviewOK  *bool          `json:"review_result"`
	Replies   []upstreamNode `json:"replies"`
}

type upstreamTree struct {
	Prompt upstreamNode `json:"prompt"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type outputRow struct {
	Messages []message `json:"messages"`
}

func main() {
	source := flag.String("source", defaultSource, "OASST1 ready trees .jsonl.gz URL or local path")
	output := flag.String("output", "pkg/bench/dataset/data/multiturn.jsonl.gz", "output gzip path")
	budget := flag.Int("max-compressed-bytes", 2500000, "maximum output size")
	flag.Parse()
	reader, closeFn, err := openSource(*source)
	if err != nil {
		fatal(err)
	}
	defer closeFn()
	gz, err := gzip.NewReader(reader)
	if err != nil {
		fatal(err)
	}
	defer gz.Close()

	var rows []outputRow
	scanner := bufio.NewScanner(gz)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		var tree upstreamTree
		if json.Unmarshal(scanner.Bytes(), &tree) != nil {
			continue
		}
		path := bestPath(tree.Prompt)
		if len(path) < 2 {
			continue
		}
		messages := make([]message, 0, len(path))
		for _, node := range path {
			role := map[string]string{"prompter": "user", "assistant": "assistant"}[node.Role]
			if role != "" && node.Text != "" {
				messages = append(messages, message{Role: role, Content: node.Text})
			}
		}
		if len(messages) >= 2 {
			rows = append(rows, outputRow{Messages: messages})
		}
	}
	if err := scanner.Err(); err != nil {
		fatal(err)
	}
	if len(rows) == 0 {
		fatal(fmt.Errorf("source produced no conversations"))
	}

	low, high := 1, len(rows)
	var best []byte
	for low <= high {
		mid := low + (high-low)/2
		data, err := compressRows(rows[:mid])
		if err != nil {
			fatal(err)
		}
		if len(data) <= *budget {
			best = data
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if len(best) == 0 {
		fatal(fmt.Errorf("compressed budget %d cannot fit one row", *budget))
	}
	if err := os.WriteFile(*output, best, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %d OASST1 conversations to %s (%d compressed bytes)\n", high, *output, len(best))
}

func openSource(source string) (io.Reader, func(), error) {
	if len(source) >= 8 && (source[:7] == "http://" || source[:8] == "https://") {
		client := &http.Client{Timeout: 10 * time.Minute}
		//nolint:bodyclose // The returned cleanup callback owns the successful body; errors close it below.
		resp, err := client.Get(source) // #nosec G107 -- operator-selected dataset source
		if err != nil {
			return nil, func() {}, err
		}
		if resp.StatusCode/100 != 2 {
			_ = resp.Body.Close()
			return nil, func() {}, fmt.Errorf("download %s: %s", source, resp.Status)
		}
		return resp.Body, func() { _ = resp.Body.Close() }, nil
	}
	f, err := os.Open(source)
	if err != nil {
		return nil, func() {}, err
	}
	return f, func() { _ = f.Close() }, nil
}

func bestPath(node upstreamNode) []upstreamNode {
	if node.Deleted || node.Text == "" || (node.ReviewOK != nil && !*node.ReviewOK) {
		return nil
	}
	replies := append([]upstreamNode(nil), node.Replies...)
	sort.SliceStable(replies, func(i, j int) bool {
		ri, rj := 1<<30, 1<<30
		if replies[i].Rank != nil {
			ri = *replies[i].Rank
		}
		if replies[j].Rank != nil {
			rj = *replies[j].Rank
		}
		return ri < rj
	})
	var child []upstreamNode
	for _, reply := range replies {
		candidate := bestPath(reply)
		if len(candidate) > len(child) {
			child = candidate
		}
		if reply.Rank != nil && *reply.Rank == 0 && len(candidate) >= 3 {
			break
		}
	}
	return append([]upstreamNode{node}, child...)
}

func compressRows(rows []outputRow) ([]byte, error) {
	var out bytes.Buffer
	gz, err := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	gz.Header.Name = "multiturn.jsonl"
	encoder := json.NewEncoder(gz)
	for _, row := range rows {
		if err := encoder.Encode(row); err != nil {
			return nil, err
		}
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
