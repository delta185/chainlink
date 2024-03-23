package remote_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	commoncap "github.com/smartcontractkit/chainlink-common/pkg/capabilities"
	"github.com/smartcontractkit/chainlink/v2/core/capabilities/remote"
	"github.com/smartcontractkit/chainlink/v2/core/capabilities/remote/types"
	remoteMocks "github.com/smartcontractkit/chainlink/v2/core/capabilities/remote/types/mocks"
	"github.com/smartcontractkit/chainlink/v2/core/internal/testutils"
	"github.com/smartcontractkit/chainlink/v2/core/logger"
	p2ptypes "github.com/smartcontractkit/chainlink/v2/core/services/p2p/types"
)

func TestTriggerPublisher_Receive(t *testing.T) {
	lggr := logger.TestLogger(t)
	ctx := testutils.Context(t)
	capInfo := commoncap.CapabilityInfo{
		ID:             "cap_id",
		CapabilityType: commoncap.CapabilityTypeTrigger,
		Description:    "Remote Trigger",
		Version:        "0.0.1",
	}
	capDonInfo := types.DON{
		ID:      "capability-don",
		Members: []p2ptypes.PeerID{{}},
	}
	workflowDonInfo := types.DON{
		ID:      "workflow-don",
		Members: []p2ptypes.PeerID{{}},
	}
	dispatcher := remoteMocks.NewDispatcher(t)
	config := types.RemoteTriggerConfig{
		RegistrationRefreshMs:   100,
		RegistrationExpiryMs:    100,
		MinResponsesToAggregate: 1,
		MessageExpiryMs:         100_000,
	}
	workflowDONs := map[string]types.DON{
		workflowDonInfo.ID: workflowDonInfo,
	}
	publisher := remote.NewTriggerPublisher(config, capInfo, capDonInfo, workflowDONs, dispatcher, lggr)

	require.NoError(t, publisher.Start(ctx))
	require.NoError(t, publisher.Close())
}
