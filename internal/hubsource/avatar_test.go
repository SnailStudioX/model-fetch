// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hubsource

import (
	"strings"
	"testing"
)

// The search APIs report no logo, so these pin the per-author lookup shape each
// source answers, and the mirror host rule that keeps the returned image URL
// loadable through the configured endpoint.
func TestAvatarURLs(t *testing.T) {
	hf := NewHF("https://hf-mirror.com")
	got := hf.AvatarURLs("manyeyes", "manyeyes/model")
	want := []string{
		"https://hf-mirror.com/api/users/manyeyes/avatar",
		"https://hf-mirror.com/api/organizations/manyeyes/avatar",
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("hf avatar routes: got %v, want %v", got, want)
	}
	if got := hf.AvatarURLs("a b/c", "a b/c/model"); !strings.Contains(got[0], "/api/users/a%20b%2Fc/avatar") {
		t.Errorf("hf avatar route must escape the author: %s", got[0])
	}

	ms := NewModelScope("")
	got = ms.AvatarURLs("iic", "iic/speech_fsmn_vad_zh-cn-16k-common-pytorch")
	if len(got) != 1 || got[0] != "https://modelscope.cn/api/v1/models/iic/speech_fsmn_vad_zh-cn-16k-common-pytorch" {
		t.Errorf("modelscope avatar route: got %v", got)
	}
	// Without a repository there is nothing to fetch: ModelScope has no anonymous
	// per-author route, so an empty sample must not produce a URL.
	if got := ms.AvatarURLs("iic", ""); len(got) != 0 {
		t.Errorf("modelscope avatar route without a sample repo: got %v", got)
	}
}

func TestParseAvatarPinsMirrorPathsOnly(t *testing.T) {
	hf := NewHF("https://hf-mirror.com")

	// The /avatars/ tree is proxied by the mirror, so the canonical-host URL is
	// pinned to the configured endpoint.
	got, err := hf.ParseAvatar(strings.NewReader(`{"avatarUrl":"https://huggingface.co/avatars/29a7ce.svg"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://hf-mirror.com/avatars/29a7ce.svg" {
		t.Errorf("proxied tree: got %s", got)
	}

	// The cdn-avatars upload tree is served by the mirror's own cdn-avatars
	// subdomain, so the logo of an org that uploaded one still loads.
	got, err = hf.ParseAvatar(strings.NewReader(`{"avatarUrl":"https://cdn-avatars.huggingface.co/v1/production/uploads/abc.jpg"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://cdn-avatars.hf-mirror.com/v1/production/uploads/abc.jpg" {
		t.Errorf("upload tree via mirror subdomain: got %s", got)
	}

	// Other hosts the mirror is not known to proxy keep their original host.
	got, err = hf.ParseAvatar(strings.NewReader(`{"avatarUrl":"https://avatars.githubusercontent.com/u/123?v=4"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://avatars.githubusercontent.com/u/123?v=4" {
		t.Errorf("unpinned third-party host: got %s", got)
	}

	// The default endpoint is the mirror, so the /avatars/ tree of a canonical URL
	// is pinned to it: hf-mirror.com is the host a fresh install can reach.
	got, err = NewHF("").ParseAvatar(strings.NewReader(`{"avatarUrl":"https://huggingface.co/avatars/29a7ce.svg"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://hf-mirror.com/avatars/29a7ce.svg" {
		t.Errorf("default endpoint: got %s", got)
	}

	// An endpoint that already is the canonical host has nothing to rewrite.
	got, err = NewHF("https://huggingface.co").ParseAvatar(strings.NewReader(`{"avatarUrl":"https://huggingface.co/avatars/29a7ce.svg"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://huggingface.co/avatars/29a7ce.svg" {
		t.Errorf("canonical endpoint: got %s", got)
	}

	if got, err := hf.ParseAvatar(strings.NewReader(`{"avatarUrl":""}`)); err != nil || got != "" {
		t.Errorf("empty avatarUrl: got %q err %v", got, err)
	}
	if _, err := hf.ParseAvatar(strings.NewReader(`not json`)); err == nil {
		t.Error("non-JSON body must report a decode error")
	}
}

func TestParseAvatarModelScopeEnvelope(t *testing.T) {
	ms := NewModelScope("")

	// The organization logo is what the site shows for a repository author, so it
	// wins over the model-level avatar.
	got, err := ms.ParseAvatar(strings.NewReader(`{"Data":{"Avatar":"https://img.alicdn.com/model.png","Organization":{"Name":"iic","Avatar":"https://resouces.modelscope.cn/avatar/86fc4b9c.png"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://resouces.modelscope.cn/avatar/86fc4b9c.png" {
		t.Errorf("organization logo: got %s", got)
	}

	// An author with no organization logo falls back to the model-level avatar.
	got, err = ms.ParseAvatar(strings.NewReader(`{"Data":{"Avatar":"https://img.alicdn.com/model.png"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://img.alicdn.com/model.png" {
		t.Errorf("model avatar fallback: got %s", got)
	}

	// A repository whose author has no logo at all is a normal empty result, not
	// an error.
	got, err = ms.ParseAvatar(strings.NewReader(`{"Data":{"Name":"iic/x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("no logo: got %q", got)
	}
}
