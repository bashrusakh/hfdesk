// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"testing"
)

// Node's built-in VM drives the shipped app.js navigation/history/classifier
// code with DOM and fetch stubs. The server-rendered README HTML is produced by
// the real renderer so the harness can compare both link-classification paths.
func TestNavigationUIHistory(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: run testdata/navigation-ui.cjs with Node to verify navigation UI")
	}

	const (
		endpoint = "https://huggingface.co"
		baseRaw  = endpoint + "/owner/model/resolve/main/"
		baseBlob = endpoint + "/owner/model/blob/main/"
	)
	markdown := "[model](" + endpoint + "/owner/model)\n\n" +
		"[dataset](" + endpoint + "/datasets/owner/data)\n\n" +
		"[dev](" + endpoint + "/owner/model/tree/dev)\n\n" +
		"[encoded](" + endpoint + "/owner/model/tree/refs%2Fpr%2F1)\n\n" +
		"[nested](" + endpoint + "/owner/model/tree/main/folder/file.md)\n\n" +
		"[dataset-nested](" + endpoint + "/datasets/owner/data/tree/v1/train)\n\n" +
		"[hardware](" + endpoint + "/hardware/a10)\n\n" +
		"[inference](" + endpoint + "/inference/models)\n\n" +
		"[solutions](" + endpoint + "/solutions/models)\n\n" +
		"[chat](" + endpoint + "/chat/assistants)\n\n" +
		"[file](config.json)\n\n" +
		"[external](https://example.com/owner/model)\n\n" +
		"[query](https://example.com/?a=1&b=2)\n"

	const (
		pathEndpoint = "https://mirror.example/hf"
		pathBaseRaw  = pathEndpoint + "/owner/model/resolve/main/"
		pathBaseBlob = pathEndpoint + "/owner/model/blob/main/"
	)
	pathMarkdown := "[model](" + pathEndpoint + "/owner/model)\n\n" +
		"[dataset](" + pathEndpoint + "/datasets/owner/data/tree/v1)\n\n" +
		"[nested](" + pathEndpoint + "/owner/model/tree/dev/folder/file.md)\n\n" +
		"[same-host](https://mirror.example/owner/model)\n"

	reserved := make([]string, 0, len(readmeReservedModelRoutes))
	for route := range readmeReservedModelRoutes {
		reserved = append(reserved, route)
	}
	sort.Strings(reserved)

	fixture, err := json.Marshal(map[string]any{
		"endpoint":                endpoint,
		"baseRawURL":              baseRaw,
		"baseBlobURL":             baseBlob,
		"markdown":                markdown,
		"serverHtml":              renderReadmeHTML(markdown, baseRaw, baseBlob, endpoint),
		"serverReservedRoutes":    reserved,
		"pathPrefixedEndpoint":    pathEndpoint,
		"pathPrefixedBaseRawURL":  pathBaseRaw,
		"pathPrefixedBaseBlobURL": pathBaseBlob,
		"pathPrefixedMarkdown":    pathMarkdown,
		"pathPrefixedServerHtml":  renderReadmeHTML(pathMarkdown, pathBaseRaw, pathBaseBlob, pathEndpoint),
	})
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, "testdata/navigation-ui.cjs")
	cmd.Env = append(os.Environ(), "HFDESK_NAV_FIXTURE="+string(fixture))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("navigation UI regression: %v\n%s", err, out)
	}
	t.Log(string(out))
}
