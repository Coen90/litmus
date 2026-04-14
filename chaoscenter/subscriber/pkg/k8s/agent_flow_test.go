package k8s

import (
	"encoding/json"
	"fmt"
	"testing"

	"subscriber/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// mockAgentOps is a minimal mock for the agent confirmation flow.
// It tracks which methods were called and with what arguments.
type mockAgentOps struct {
	mock.Mock
}

func (m *mockAgentOps) IsAgentConfirmed() (bool, string, error) {
	args := m.Called()
	return args.Bool(0), args.String(1), args.Error(2)
}

func (m *mockAgentOps) AgentRegister(accessKey string) (bool, error) {
	args := m.Called(accessKey)
	return args.Bool(0), args.Error(1)
}

func (m *mockAgentOps) AgentConfirm(infraData map[string]string) ([]byte, error) {
	args := m.Called(infraData)
	return args.Get(0).([]byte), args.Error(1)
}

// agentOps defines the minimal interface needed for the agent confirmation flow.
// This matches the subset of SubscriberK8s used in subscriber.go init() lines 104-135.
type agentOps interface {
	IsAgentConfirmed() (bool, string, error)
	AgentRegister(accessKey string) (bool, error)
	AgentConfirm(infraData map[string]string) ([]byte, error)
}

// handleAgentConfirmation replicates the exact logic from subscriber.go:104-135.
// This is a 1:1 copy of the current (buggy) production code, extracted for testability.
//
// BUG: When isConfirmed == true, the function only updates infraData in memory
// but does NOT call AgentRegister() to ensure ConfigMap/Secret are up-to-date.
// This causes pods to lose connectivity after restarts or ArgoCD syncs.
func handleAgentConfirmation(ops agentOps, infraData map[string]string) error {
	isConfirmed, newKey, err := ops.IsAgentConfirmed()
	if err != nil {
		return fmt.Errorf("failed to check agent confirmed status: %w", err)
	}

	if isConfirmed {
		// Current code: only updates in-memory map, does NOT call AgentRegister
		infraData["ACCESS_KEY"] = newKey
	} else if !isConfirmed {
		infraConfirmByte, err := ops.AgentConfirm(infraData)
		if err != nil {
			return fmt.Errorf("failed to confirm agent: %w", err)
		}

		var infraConfirmInterface types.Payload
		err = json.Unmarshal(infraConfirmByte, &infraConfirmInterface)
		if err != nil {
			return fmt.Errorf("failed to parse agent confirm data: %w", err)
		}

		if infraConfirmInterface.Data.InfraConfirm.IsInfraConfirmed {
			infraData["ACCESS_KEY"] = infraConfirmInterface.Data.InfraConfirm.NewAccessKey
			infraData["IS_INFRA_CONFIRMED"] = "true"

			_, err = ops.AgentRegister(infraData["ACCESS_KEY"])
			if err != nil {
				return fmt.Errorf("failed to register agent: %w", err)
			}
		}
	}
	return nil
}

// =============================================================================
// Failing tests: These demonstrate the bug described in GitHub issue #5461.
// All tests in this section FAIL with the current code and should PASS after fix.
// =============================================================================

// TestAgentAlreadyConfirmed_ShouldCallAgentRegister demonstrates the core bug:
// when IsAgentConfirmed() returns true, AgentRegister() is never called,
// so ConfigMap/Secret are not refreshed after restarts or re-syncs.
func TestAgentAlreadyConfirmed_ShouldCallAgentRegister(t *testing.T) {
	// ARRANGE
	mockOps := new(mockAgentOps)
	mockOps.On("IsAgentConfirmed").Return(true, "existing-access-key", nil)
	mockOps.On("AgentRegister", "existing-access-key").Return(true, nil)

	infraData := map[string]string{
		"INFRA_ID": "infra-123",
	}

	// ACT
	err := handleAgentConfirmation(mockOps, infraData)

	// ASSERT
	assert.NoError(t, err)
	assert.Equal(t, "existing-access-key", infraData["ACCESS_KEY"])

	// BUG: This FAILS because AgentRegister is never called when isConfirmed == true.
	// ConfigMap (subscriber-config) and Secret (subscriber-secret) are not refreshed,
	// causing pod connectivity failure after restart or ArgoCD sync.
	mockOps.AssertCalled(t, "AgentRegister", "existing-access-key")
}

// TestAgentAlreadyConfirmed_ShouldSetIsInfraConfirmed demonstrates a secondary bug:
// IS_INFRA_CONFIRMED is not set in infraData when the agent is already confirmed.
// The !isConfirmed path sets it to "true", but the isConfirmed path skips it entirely.
func TestAgentAlreadyConfirmed_ShouldSetIsInfraConfirmed(t *testing.T) {
	// ARRANGE
	mockOps := new(mockAgentOps)
	mockOps.On("IsAgentConfirmed").Return(true, "existing-access-key", nil)
	mockOps.On("AgentRegister", "existing-access-key").Return(true, nil)

	infraData := map[string]string{
		"INFRA_ID": "infra-123",
		// IS_INFRA_CONFIRMED is not set (simulating missing env var after restart)
	}

	// ACT
	_ = handleAgentConfirmation(mockOps, infraData)

	// ASSERT
	// BUG: This FAILS because the isConfirmed branch does not set IS_INFRA_CONFIRMED.
	// When the subscriber restarts and the env var is lost, this field stays empty.
	assert.Equal(t, "true", infraData["IS_INFRA_CONFIRMED"])
}

// TestAgentReSync_ConfigMapDeletedThenRestarted simulates the exact scenario
// reported in issue #5461:
// 1. Agent was initially registered (configs exist)
// 2. ArgoCD sync or restart deletes/resets ConfigMap and Secret
// 3. Subscriber restarts and calls IsAgentConfirmed()
// 4. IsAgentConfirmed() returns error because ConfigMap is missing
// 5. The flow should recover by re-registering, but Fatal() is called instead
func TestAgentReSync_ConfigMapDeletedThenRestarted(t *testing.T) {
	// ARRANGE: Simulate ConfigMap not found (deleted by ArgoCD sync)
	mockOps := new(mockAgentOps)
	mockOps.On("IsAgentConfirmed").
		Return(false, "", fmt.Errorf("subscriber-config configmap not found"))

	// The flow should fall through to AgentConfirm when IsAgentConfirmed returns error,
	// but current code calls Fatal instead of attempting recovery.
	confirmResponse := types.Payload{
		Data: types.Data{
			InfraConfirm: types.InfraConfirm{
				IsInfraConfirmed: true,
				NewAccessKey:     "recovered-key",
				InfraID:          "infra-123",
			},
		},
	}
	confirmBytes, _ := json.Marshal(confirmResponse)
	mockOps.On("AgentConfirm", mock.Anything).Return(confirmBytes, nil)
	mockOps.On("AgentRegister", "recovered-key").Return(true, nil)

	infraData := map[string]string{
		"INFRA_ID":    "infra-123",
		"ACCESS_KEY":  "old-key",
		"SERVER_ADDR": "http://chaos-center:9002",
	}

	// ACT
	err := handleAgentConfirmation(mockOps, infraData)

	// ASSERT
	// BUG: This FAILS. When IsAgentConfirmed() returns an error (ConfigMap missing),
	// the current code calls logrus.Fatal() instead of attempting re-registration.
	// handleAgentConfirmation returns the error, and AgentConfirm is never called.
	assert.NoError(t, err, "should recover from missing ConfigMap by re-confirming")
	mockOps.AssertCalled(t, "AgentConfirm", mock.Anything)
	mockOps.AssertCalled(t, "AgentRegister", "recovered-key")
}

// =============================================================================
// Passing tests: These verify the current behavior for reference.
// =============================================================================

// TestAgentFirstRegistration_ShouldCallAgentRegister verifies that the first-time
// registration flow works correctly. This path is NOT affected by the bug.
func TestAgentFirstRegistration_ShouldCallAgentRegister(t *testing.T) {
	// ARRANGE: Agent not confirmed yet
	mockOps := new(mockAgentOps)
	mockOps.On("IsAgentConfirmed").Return(false, "", nil)

	confirmResponse := types.Payload{
		Data: types.Data{
			InfraConfirm: types.InfraConfirm{
				IsInfraConfirmed: true,
				NewAccessKey:     "new-access-key",
				InfraID:          "infra-123",
			},
		},
	}
	confirmBytes, _ := json.Marshal(confirmResponse)
	mockOps.On("AgentConfirm", mock.Anything).Return(confirmBytes, nil)
	mockOps.On("AgentRegister", "new-access-key").Return(true, nil)

	infraData := map[string]string{
		"INFRA_ID":    "infra-123",
		"SERVER_ADDR": "http://chaos-center:9002",
	}

	// ACT
	err := handleAgentConfirmation(mockOps, infraData)

	// ASSERT: First registration works correctly
	assert.NoError(t, err)
	assert.Equal(t, "new-access-key", infraData["ACCESS_KEY"])
	assert.Equal(t, "true", infraData["IS_INFRA_CONFIRMED"])
	mockOps.AssertCalled(t, "AgentConfirm", mock.Anything)
	mockOps.AssertCalled(t, "AgentRegister", "new-access-key")
}

// TestAgentFirstRegistration_ServerRejectsConfirm verifies behavior when
// the Chaos Center says the agent is not confirmed.
func TestAgentFirstRegistration_ServerRejectsConfirm(t *testing.T) {
	// ARRANGE
	mockOps := new(mockAgentOps)
	mockOps.On("IsAgentConfirmed").Return(false, "", nil)

	confirmResponse := types.Payload{
		Data: types.Data{
			InfraConfirm: types.InfraConfirm{
				IsInfraConfirmed: false,
			},
		},
	}
	confirmBytes, _ := json.Marshal(confirmResponse)
	mockOps.On("AgentConfirm", mock.Anything).Return(confirmBytes, nil)

	infraData := map[string]string{
		"INFRA_ID": "infra-123",
	}

	// ACT
	err := handleAgentConfirmation(mockOps, infraData)

	// ASSERT: AgentRegister should NOT be called when server rejects
	assert.NoError(t, err)
	mockOps.AssertCalled(t, "AgentConfirm", mock.Anything)
	mockOps.AssertNotCalled(t, "AgentRegister", mock.Anything)
}
