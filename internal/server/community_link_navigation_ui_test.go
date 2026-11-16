// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCommunityLinkNavigationUI checks the Community panel's link routing with
// the real sanitizer output. Comment HTML is sanitized server-side and keeps
// each anchor's external href while dropping repository-navigation attributes,
// so the panel must classify repository roots from the href when they are
// clicked. The Node harness drives the shipped app.js delegation with anchors
// rendered by the actual sanitizer.
func TestCommunityLinkNavigationUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: run testdata/community-link-navigation-ui.cjs with Node to verify community link routing")
	}

	const (
		endpoint = "https://huggingface.co"
		baseRaw  = endpoint + "/owner/model/resolve/main/"
		baseBlob = endpoint + "/owner/model/blob/main/"
	)
	externalHrefs := []string{
		endpoint + "/owner/model/blob/main/config.json",
		endpoint + "/owner/model/tree/main",
		endpoint + "/owner/model/resolve/main/config.json",
		endpoint + "/owner/model/commit/abc123",
		endpoint + "/papers/2401.04088",
		"https://example.com/owner/model",
		"mailto:someone@example.com",
	}
	markdown := "[model](" + endpoint + "/owner/model)\n\n" +
		"[dataset](" + endpoint + "/datasets/owner/data)\n\n"
	for _, href := range externalHrefs {
		markdown += "[external](" + href + ")\n\n"
	}
	// A comment must not be able to forge in-app navigation attributes either.
	markdown += `<a href="https://example.com/forged" data-hf-repo="owner/model" data-hf-repo-type="model" data-hf-repo-revision="dev">forged</a>` + "\n"

	communityHTML := renderCommunityCommentHTML(markdown, "", baseRaw, baseBlob, endpoint)
	for _, attr := range []string{"data-hf-repo=", "data-hf-repo-type=", "data-hf-repo-revision="} {
		if strings.Contains(communityHTML, attr) {
			t.Fatalf("sanitized Community HTML must keep %s stripped:\n%s", attr, communityHTML)
		}
	}
	for _, href := range append([]string{
		endpoint + "/owner/model",
		endpoint + "/datasets/owner/data",
	}, externalHrefs...) {
		if !strings.Contains(communityHTML, `href="`+href+`"`) {
			t.Fatalf("sanitized Community HTML must keep the external href %q:\n%s", href, communityHTML)
		}
	}

	const (
		pathEndpoint = "https://mirror.example/hf"
		pathBaseRaw  = pathEndpoint + "/owner/model/resolve/main/"
		pathBaseBlob = pathEndpoint + "/owner/model/blob/main/"
	)
	pathMarkdown := "[model](" + pathEndpoint + "/owner/model)\n\n" +
		"[dataset](" + pathEndpoint + "/datasets/owner/data)\n\n" +
		"[same-host](https://mirror.example/owner/model)\n\n" +
		"[tree](" + pathEndpoint + "/owner/model/tree/dev)\n"
	pathCommunityHTML := renderCommunityCommentHTML(pathMarkdown, "", pathBaseRaw, pathBaseBlob, pathEndpoint)

	fixture, err := json.Marshal(map[string]any{
		"endpoint":                  endpoint,
		"communityHtml":             communityHTML,
		"pathEndpoint":              pathEndpoint,
		"pathPrefixedCommunityHtml": pathCommunityHTML,
	})
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, "testdata/community-link-navigation-ui.cjs")
	cmd.Env = append(os.Environ(), "HFDESK_COMMUNITY_FIXTURE="+string(fixture))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("community link navigation regression: %v\n%s", err, out)
	}
	t.Log(string(out))
}
