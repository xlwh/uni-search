// Package chaos chaos_test.go tests chaos scenario definitions and
// validation logic. Real Docker+toxiproxy execution requires a cluster
// environment; these tests validate the scenario framework, report
// generation, and invariants.
// @spec: ./active_specs/2026-10-20-gap-fill/02_spec.md#2.6
package chaos

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAllScenarios_Completeness(t *testing.T) {
	scenarios := AllScenarios()
	assert.Len(t, scenarios, 6, "should have C1-C6 scenarios")

	ids := make(map[string]bool)
	for _, s := range scenarios {
		ids[s.ID] = true
		assert.NotEmpty(t, s.Name)
		assert.NotEmpty(t, s.Description)
		assert.NotEmpty(t, s.Expected)
		assert.Greater(t, s.Duration, time.Duration(0))
	}

	for _, id := range []string{"C1", "C2", "C3", "C4", "C5", "C6"} {
		assert.True(t, ids[id], "scenario %s should exist", id)
	}
}

func TestScenario_FaultTypes(t *testing.T) {
	scenarios := AllScenarios()
	faultTypes := make(map[FaultType]bool)
	for _, s := range scenarios {
		faultTypes[s.FaultType] = true
	}

	// Verify all 6 fault types are represented.
	assert.True(t, faultTypes[FaultKill], "should have FaultKill")
	assert.True(t, faultTypes[FaultPartition], "should have FaultPartition")
	assert.True(t, faultTypes[FaultSlowIO], "should have FaultSlowIO")
	assert.True(t, faultTypes[FaultDualKill], "should have FaultDualKill")
	assert.True(t, faultTypes[FaultRetry], "should have FaultRetry")
	assert.True(t, faultTypes[FaultSplitBrain], "should have FaultSplitBrain")
}

func TestDockerComposeConfig_ValidYAML(t *testing.T) {
	config := DockerComposeConfig()
	assert.Contains(t, config, "version: '3.8'")
	assert.Contains(t, config, "meta-1")
	assert.Contains(t, config, "meta-2")
	assert.Contains(t, config, "meta-3")
	assert.Contains(t, config, "shard-1")
	assert.Contains(t, config, "shard-2")
	assert.Contains(t, config, "coordinator")
	assert.Contains(t, config, "minio")
	assert.Contains(t, config, "toxiproxy")
}

func TestToxiproxyConfig_ValidJSON(t *testing.T) {
	config := ToxiproxyConfig()
	assert.Contains(t, config, "meta-link")
	assert.Contains(t, config, "s3-link")
	assert.Contains(t, config, "8400")
	assert.Contains(t, config, "9000")
}

func TestRunScenario_ReturnsReport(t *testing.T) {
	scenarios := AllScenarios()
	for _, s := range scenarios {
		report := RunScenario(s)
		assert.Equal(t, s.ID, report.ScenarioID)
		assert.NotEmpty(t, report.Details)
	}
}

func TestRunAllScenarios_AllPassed(t *testing.T) {
	reports := RunAllScenarios()
	assert.Len(t, reports, 6, "should have 6 reports")

	for _, r := range reports {
		assert.True(t, r.Passed, "scenario %s should pass", r.ScenarioID)
	}
}

func TestPrintReport_Format(t *testing.T) {
	reports := RunAllScenarios()
	output := PrintReport(reports)

	assert.Contains(t, output, "Chaos Test Report")
	assert.Contains(t, output, "PASS")
	assert.Contains(t, output, "ALL GREEN")

	// Verify all scenario IDs are in the report.
	for _, r := range reports {
		assert.Contains(t, output, r.ScenarioID)
	}
}

func TestScenario_C1_KillLeader(t *testing.T) {
	scenarios := AllScenarios()
	var c1 *Scenario
	for i := range scenarios {
		if scenarios[i].ID == "C1" {
			c1 = &scenarios[i]
			break
		}
	}
	if c1 == nil {
		t.Fatal("C1 scenario not found")
	}

	assert.Equal(t, "kill-leader", c1.Name)
	assert.Equal(t, FaultKill, c1.FaultType)
	assert.Contains(t, c1.Expected, "RTO")
}

func TestScenario_C4_DualKillDuringSplit(t *testing.T) {
	scenarios := AllScenarios()
	var c4 *Scenario
	for i := range scenarios {
		if scenarios[i].ID == "C4" {
			c4 = &scenarios[i]
			break
		}
	}
	if c4 == nil {
		t.Fatal("C4 scenario not found")
	}

	assert.Equal(t, "split-dual-kill", c4.Name)
	assert.Equal(t, FaultDualKill, c4.FaultType)
	assert.Contains(t, c4.Expected, "zero-disruption")
}

func TestScenarioReport_Failure(t *testing.T) {
	// Verify that a failed scenario report can be properly represented.
	report := ScenarioReport{
		ScenarioID:     "C1",
		Passed:         false,
		Duration:       0,
		Details:        "RTO exceeded 30s",
		DataConsistent: false,
	}

	output := PrintReport([]ScenarioReport{report})
	assert.Contains(t, output, "FAIL")
	assert.Contains(t, output, "RTO exceeded")
	assert.Contains(t, strings.ReplaceAll(output, " ", ""), "FAILURES")
}
