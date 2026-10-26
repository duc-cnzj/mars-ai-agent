package main

import (
	"strings"
	"testing"
)

// TestSelectBackend_Default 验证未设置 MARS_AGENT_BACKEND 时回落到 opencode。
func TestSelectBackend_Default(t *testing.T) {
	backend, err := SelectBackend(Env{})
	if err != nil {
		t.Fatalf("SelectBackend() error = %v", err)
	}
	if backend.Name() != "opencode" {
		t.Errorf("backend.Name() = %q, want opencode", backend.Name())
	}
}

// TestSelectBackend_Explicit 验证显式选择 opencode。
func TestSelectBackend_Explicit(t *testing.T) {
	backend, err := SelectBackend(Env{Backend: "opencode"})
	if err != nil {
		t.Fatalf("SelectBackend() error = %v", err)
	}
	if backend.Name() != "opencode" {
		t.Errorf("backend.Name() = %q, want opencode", backend.Name())
	}
}

// TestSelectBackend_Unknown 验证未知内核 fail fast，并列出可选值。
func TestSelectBackend_Unknown(t *testing.T) {
	_, err := SelectBackend(Env{Backend: "claude"})
	if err == nil {
		t.Fatal("SelectBackend() error = nil, want error")
	}
	for _, want := range []string{"claude", "opencode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息 %q 未包含 %q", err.Error(), want)
		}
	}
}

// TestOpencodeCapabilities 验证 P0 opencode 实现声明的能力位。
func TestOpencodeCapabilities(t *testing.T) {
	caps := (opencodeBackend{}).Capabilities()
	if !caps.Server || !caps.Attach || !caps.MCPProbe || !caps.SessionResume || !caps.Branding {
		t.Errorf("opencode Capabilities() = %+v, want 全部 true", caps)
	}
}

// TestRegisteredBackends 验证注册表当前只有 opencode。
func TestRegisteredBackends(t *testing.T) {
	got := registeredBackends()
	if len(got) != 1 || got[0] != "opencode" {
		t.Errorf("registeredBackends() = %v, want [opencode]", got)
	}
}
