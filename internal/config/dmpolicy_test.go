package config

import "testing"

func TestCompanionConfig_AllowsDMFrom(t *testing.T) {
	t.Parallel()

	const senderKey = "AABBCCDDEEFF00112233445566778899aabbccddeeff00112233445566778899"

	policy := func(p string) *string { return &p }
	list := func(k ...string) *[]string { return &k }

	tests := []struct {
		name      string
		cfg       CompanionConfig
		isContact bool
		want      bool
	}{
		{"unset policy keeps the pre-column behaviour: contacts only", CompanionConfig{}, true, true},
		{"unset policy rejects a stranger", CompanionConfig{}, false, false},
		{"contacts accepts a contact", CompanionConfig{DMPolicy: policy(DMPolicyContacts)}, true, true},
		{"contacts rejects a stranger", CompanionConfig{DMPolicy: policy(DMPolicyContacts)}, false, false},
		{"anyone accepts a stranger", CompanionConfig{DMPolicy: policy(DMPolicyAnyone)}, false, true},
		{
			"allowlist accepts a listed full key",
			CompanionConfig{DMPolicy: policy(DMPolicyAllowlist), DMAllow: list(senderKey)},
			false, true,
		},
		{
			"allowlist accepts a listed prefix, which is what the UI shows",
			CompanionConfig{DMPolicy: policy(DMPolicyAllowlist), DMAllow: list("aabbccddeeff")},
			false, true,
		},
		{
			"allowlist rejects an unlisted key even when it is a contact",
			CompanionConfig{DMPolicy: policy(DMPolicyAllowlist), DMAllow: list("0011223344")},
			true, false,
		},
		{
			"an empty allowlist rejects everyone rather than defaulting open",
			CompanionConfig{DMPolicy: policy(DMPolicyAllowlist)},
			true, false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.cfg.AllowsDMFrom(senderKey, tt.isContact); got != tt.want {
				t.Errorf("AllowsDMFrom(isContact=%v) = %v, want %v", tt.isContact, got, tt.want)
			}
		})
	}
}

func TestValidate_DMPolicy(t *testing.T) {
	t.Parallel()

	bad := "friends"
	cfg := &Config{Companions: []CompanionConfig{{Name: "bot", DMPolicy: &bad}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("an unknown dmPolicy must be rejected, or it would silently fall through to contacts")
	}

	oddHex := []string{"abc"}
	cfg = &Config{Companions: []CompanionConfig{{Name: "bot", DMAllow: &oddHex}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("an odd-length dmAllow entry must be rejected: it could never match a sender")
	}

	ok := DMPolicyAnyone
	good := []string{"AABBCC"}
	cfg = &Config{Companions: []CompanionConfig{{Name: "bot", DMPolicy: &ok, DMAllow: &good}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a valid policy was rejected: %v", err)
	}
}
