package safety

import "testing"

// sampleRules mirrors the storage red-lines a domain.toml would declare.
func sampleFilter(t *testing.T) *Filter {
	t.Helper()
	f, err := NewFromSpecs([]RuleSpec{
		{ID: "ctl-force-promote", Severity: SeverityCritical, Pattern: `\bstoragectl\b.*\bpromote\b.*--force`, Reason: "force-promote → split-brain"},
		{ID: "flag-overwrite", Severity: SeverityHigh, Pattern: `--overwrite`, Reason: "overwrites peer"},
		{ID: "wipefs", Severity: SeverityCritical, Pattern: `\bwipefs\b`, Reason: "erases fs signatures"},
		{ID: "lvremove", Severity: SeverityCritical, Pattern: `\blvremove\b`, Reason: "deletes LV"},
		{ID: "zfs-destroy", Severity: SeverityCritical, Pattern: `\bzfs\b.*\bdestroy\b`, Reason: "destroys dataset"},
	})
	if err != nil {
		t.Fatalf("compile specs: %v", err)
	}
	return f
}

func TestEmptyFilterBlocksNothing(t *testing.T) {
	f, _ := NewFromSpecs(nil)
	if f.Check("storagectl promote --force res0").Blocked {
		t.Fatal("a domain with no red-lines must block nothing")
	}
}

func TestFilter_SafeCommandsPass(t *testing.T) {
	f := sampleFilter(t)
	for _, cmd := range []string{
		"storagectl status res0", "storagectl node list", "cat /proc/mounts", "lsblk", "vgs",
		"storagectl resource create node_b res_data --diskless",
	} {
		if v := f.Check(cmd); v.Blocked {
			t.Errorf("expected SAFE, blocked: %q (rule=%s)", cmd, v.RuleID)
		}
	}
}

func TestFilter_DangerousCommandsBlocked(t *testing.T) {
	f := sampleFilter(t)
	cases := []struct {
		cmd, rule string
		critical  bool
	}{
		{"storagectl promote --force res0", "ctl-force-promote", true},
		{"storagectl -- --overwrite-data-of-peer promote res0", "flag-overwrite", false},
		{"wipefs -a /dev/sdb", "wipefs", true},
		{"lvremove vg0/lv_data", "lvremove", true},
		{"zfs destroy tank/res0", "zfs-destroy", true},
	}
	for _, c := range cases {
		v := f.Check(c.cmd)
		if !v.Blocked || v.RuleID != c.rule {
			t.Errorf("%q: got blocked=%v rule=%s, want rule=%s", c.cmd, v.Blocked, v.RuleID, c.rule)
		}
		if v.RequiresUnlockKey != c.critical {
			t.Errorf("%q: RequiresUnlockKey=%v want %v", c.cmd, v.RequiresUnlockKey, c.critical)
		}
	}
}

func TestFilter_CatchesDangerInCompoundLine(t *testing.T) {
	f := sampleFilter(t)
	v := f.Check("storagectl status res0 && lvremove -f vg0/lv_data")
	if !v.Blocked || v.RuleID != "lvremove" {
		t.Fatalf("expected lvremove blocked in compound line, got blocked=%v rule=%s", v.Blocked, v.RuleID)
	}
}
