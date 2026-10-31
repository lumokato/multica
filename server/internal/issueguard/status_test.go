package issueguard

import "testing"

func fullBrief() *TaskBrief {
	return &TaskBrief{
		Goal:        "347 的 7 话在展示端可见",
		Assertion:   "curl -s $HOST/api/chapters | jq '.chapters|length' == 7",
		Artifacts:   "world_sim@main，临时目录 /tmp/build 已清理",
		Constraints: "不改 pnpm-lock，优先级由 lumokato 定",
	}
}

func TestCheckStatusChange(t *testing.T) {
	tests := []struct {
		name      string
		in        StatusChange
		wantCodes []string // exact set of blocking codes expected, in any order
		wantWarns bool     // whether at least one warning is expected
		blocked   bool
	}{
		{
			name:    "done with no brief is blocked",
			in:      StatusChange{From: "in_progress", To: "done"},
			blocked: true,
			wantCodes: []string{
				"brief_missing",
			},
		},
		{
			name: "done with brief and passing evidence is allowed",
			in: StatusChange{
				From:     "in_progress",
				To:       "done",
				Brief:    fullBrief(),
				Risk:     RiskLow,
				Evidence: []Evidence{{Command: "make verify", ExitCode: 0}},
			},
		},
		{
			name: "done without assertion is blocked",
			in: StatusChange{
				From:     "in_progress",
				To:       "done",
				Brief:    &TaskBrief{Goal: "do the thing", Artifacts: "a", Constraints: "b"},
				Evidence: []Evidence{{Command: "make verify", ExitCode: 0}},
			},
			blocked:   true,
			wantCodes: []string{"assertion_missing"},
		},
		{
			name: "done with no evidence is blocked",
			in: StatusChange{
				From:  "in_progress",
				To:    "done",
				Brief: fullBrief(),
			},
			blocked:   true,
			wantCodes: []string{"evidence_missing"},
		},
		{
			name: "done with only failing evidence is blocked",
			in: StatusChange{
				From:     "in_progress",
				To:       "done",
				Brief:    fullBrief(),
				Evidence: []Evidence{{Command: "make verify", ExitCode: 2}},
			},
			blocked:   true,
			wantCodes: []string{"evidence_failed"},
		},
		{
			name: "evidence without command is only a warning",
			in: StatusChange{
				From:     "in_progress",
				To:       "done",
				Brief:    fullBrief(),
				Evidence: []Evidence{{Command: "", ExitCode: 0}, {Command: "make verify", ExitCode: 0}},
			},
			wantWarns: true,
		},
		{
			name: "high risk without display check is blocked",
			in: StatusChange{
				From:     "in_progress",
				To:       "done",
				Brief:    fullBrief(),
				Risk:     RiskHigh,
				Evidence: []Evidence{{Command: "make deploy", ExitCode: 0}},
			},
			blocked:   true,
			wantCodes: []string{"display_not_checked", "reviewer_missing"},
		},
		{
			name: "high risk with display and reviewer is allowed",
			in: StatusChange{
				From:             "in_progress",
				To:               "done",
				Brief:            fullBrief(),
				Risk:             RiskHigh,
				ReviewerApproved: true,
				Evidence:         []Evidence{{Command: "make deploy", ExitCode: 0, DisplayChecked: true}},
			},
		},
		{
			name: "ai is judged like done",
			in: StatusChange{
				From:  "in_review",
				To:    "ai",
				Brief: &TaskBrief{Goal: "g"},
			},
			blocked:   true,
			wantWarns: true,
			wantCodes: []string{"assertion_missing", "evidence_missing"},
		},
		{
			name:    "blocked without detail is blocked",
			in:      StatusChange{From: "in_progress", To: "blocked"},
			blocked: true,
			wantCodes: []string{
				"blocked_reason_missing",
			},
		},
		{
			name: "blocked with partial detail is blocked",
			in: StatusChange{
				From:    "in_progress",
				To:      "blocked",
				Blocked: &BlockedReason{Reason: "等证书"},
			},
			blocked:   true,
			wantCodes: []string{"blocked_dependency_missing", "blocked_wakeup_missing"},
		},
		{
			name: "blocked with all three parts is allowed",
			in: StatusChange{
				From:    "in_progress",
				To:      "blocked",
				Blocked: &BlockedReason{Reason: "等证书", DependsOn: "证书签发方", WakeupCondition: "证书文件出现在 /etc/ssl"},
			},
		},
		{
			name:      "start without brief only warns",
			in:        StatusChange{From: "todo", To: "in_progress"},
			wantWarns: true,
		},
		{
			name: "cancel is always allowed",
			in:   StatusChange{From: "blocked", To: "cancelled"},
		},
		{
			name:      "unknown status only warns",
			in:        StatusChange{From: "todo", To: "some_future_status"},
			wantWarns: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckStatusChange(tc.in)

			if tc.blocked != Blocks(got) {
				t.Fatalf("Blocks() = %v, want %v (violations: %+v)", Blocks(got), tc.blocked, got)
			}

			var gotBlocking []string
			var gotWarning bool
			for _, v := range got {
				if v.Severity == SeverityBlocking {
					gotBlocking = append(gotBlocking, v.Code)
				} else {
					gotWarning = true
				}
			}

			if len(gotBlocking) != len(tc.wantCodes) {
				t.Fatalf("blocking codes = %v, want %v", gotBlocking, tc.wantCodes)
			}
			for _, want := range tc.wantCodes {
				found := false
				for _, g := range gotBlocking {
					if g == want {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("missing blocking code %q in %v", want, gotBlocking)
				}
			}

			if tc.wantWarns && !gotWarning {
				t.Fatalf("expected at least one warning, got %+v", got)
			}
			if !tc.wantWarns && gotWarning {
				t.Fatalf("expected no warnings, got %+v", got)
			}
		})
	}
}
