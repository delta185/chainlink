package remote

import (
	"context"
	"errors"
	sync "sync"
	"time"

	commoncap "github.com/smartcontractkit/chainlink-common/pkg/capabilities"
	"github.com/smartcontractkit/chainlink-common/pkg/capabilities/pb"
	"github.com/smartcontractkit/chainlink-common/pkg/services"
	"github.com/smartcontractkit/chainlink/v2/core/capabilities/remote/types"
	"github.com/smartcontractkit/chainlink/v2/core/logger"
	p2ptypes "github.com/smartcontractkit/chainlink/v2/core/services/p2p/types"
)

// TriggerPublisher manages all external users of a local trigger capability.
// Its responsibilities are:
//  1. Manage trigger registrations from external nodes (receive, store, aggregate, expire).
//  2. Send out events produced by a concrete trigger implementation.
//
// TriggerPublisher communicates with corresponding TriggerSubscribers on remote nodes.
//
// Expected usage by a concrete trigger implementation:
//  1. Call GetActiveRegistrations() to refresh the list of active registrations
//  2. Call SendTriggerEvent() for every new event (batched by subscribed workflow IDs)
type triggerPublisher struct {
	config        types.RemoteTriggerConfig
	capInfo       commoncap.CapabilityInfo
	capDonInfo    types.DON
	workflowDONs  map[string]types.DON
	dispatcher    types.Dispatcher
	messageCache  *messageCache[registrationKey, p2ptypes.PeerID]
	registrations map[registrationKey]commoncap.CapabilityRequest
	mu            sync.RWMutex
	stopCh        services.StopChan
	wg            sync.WaitGroup
	lggr          logger.Logger
}

type registrationKey struct {
	callerDonId string
	workflowId  string
}

var _ types.Receiver = &triggerPublisher{}
var _ services.Service = &triggerPublisher{}

func NewTriggerPublisher(config types.RemoteTriggerConfig, capInfo commoncap.CapabilityInfo, capDonInfo types.DON, workflowDONs map[string]types.DON, dispatcher types.Dispatcher, lggr logger.Logger) *triggerPublisher {
	config.ApplyDefaults()
	return &triggerPublisher{
		config:        config,
		capInfo:       capInfo,
		capDonInfo:    capDonInfo,
		workflowDONs:  workflowDONs,
		dispatcher:    dispatcher,
		messageCache:  NewMessageCache[registrationKey, p2ptypes.PeerID](),
		registrations: make(map[registrationKey]commoncap.CapabilityRequest),
		stopCh:        make(services.StopChan),
		lggr:          lggr,
	}
}

func (p *triggerPublisher) Start(ctx context.Context) error {
	p.wg.Add(1)
	go p.registrationCleanupLoop()
	p.lggr.Info("TriggerPublisher started")
	return nil
}

func (p *triggerPublisher) Receive(msg *types.MessageBody) {
	sender := ToPeerID(msg.Sender)
	if msg.Method == types.MethodRegisterTrigger {
		req, err := pb.UnmarshalCapabilityRequest(msg.Payload)
		if err != nil {
			p.lggr.Errorw("failed to unmarshal capability request", "capabilityId", p.capInfo.ID, "err", err)
			return
		}
		callerDon, ok := p.workflowDONs[msg.CallerDonId]
		if !ok {
			p.lggr.Errorw("received a message from unsupported workflow DON", "capabilityId", p.capInfo.ID, "callerDonId", msg.CallerDonId)
			return
		}
		p.lggr.Debugw("received trigger registration", "capabilityId", p.capInfo.ID, "workflowId", req.Metadata.WorkflowID, "sender", sender)
		key := registrationKey{msg.CallerDonId, req.Metadata.WorkflowID}
		nowMs := time.Now().UnixMilli()
		p.mu.Lock()
		p.messageCache.Insert(key, sender, nowMs, msg.Payload)
		// NOTE: require 2F+1 by default, introduce different strategies later (KS-76)
		ready, payloads := p.messageCache.Ready(key, uint32(2*callerDon.F+1), nowMs-int64(p.config.RegistrationExpiryMs), false)
		p.mu.Unlock()
		if ready {
			return
		}
		agg := NewDefaultModeAggregator(uint32(callerDon.F + 1))
		aggregated, err := agg.Aggregate("", payloads)
		if err != nil {
			p.lggr.Errorw("failed to aggregate trigger registrations", "capabilityId", p.capInfo.ID, "workflowId", req.Metadata.WorkflowID, "err", err)
			return
		}
		unmarshaled, err := pb.UnmarshalCapabilityRequest(aggregated)
		if err != nil {
			p.lggr.Errorw("failed to unmarshal request", "capabilityId", p.capInfo.ID, "err", err)
			return
		}
		p.mu.Lock()
		p.registrations[key] = unmarshaled
		p.mu.Unlock()
		p.lggr.Debugw("updates trigger registration", "capabilityId", p.capInfo.ID, "workflowId", req.Metadata.WorkflowID)
	} else {
		p.lggr.Errorw("received trigger request with unknown method", "method", msg.Method, "sender", sender)
	}
}

func (p *triggerPublisher) registrationCleanupLoop() {
	defer p.wg.Done()
	ticker := time.NewTicker(time.Duration(p.config.RegistrationExpiryMs) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			now := time.Now().UnixMilli()
			p.mu.RLock()
			for key := range p.registrations {
				callerDon := p.workflowDONs[key.callerDonId]
				ready, _ := p.messageCache.Ready(key, uint32(2*callerDon.F+1), now-int64(p.config.RegistrationExpiryMs), false)
				if !ready {
					p.lggr.Infow("trigger registration expired", "capabilityId", p.capInfo.ID, "callerDonID", key.callerDonId, "workflowId", key.workflowId)
					delete(p.registrations, key)
					p.messageCache.Delete(key)
				}
			}
			p.mu.RUnlock()
		}
	}
}

func (p *triggerPublisher) GetActiveRegistrations() map[registrationKey]commoncap.CapabilityRequest {
	regCopy := make(map[registrationKey]commoncap.CapabilityRequest)
	p.mu.RLock()
	defer p.mu.RUnlock()
	for k, v := range p.registrations {
		regCopy[k] = v
	}
	return regCopy
}

func (p *triggerPublisher) SendTriggerEvent(destDonID string, destWorkflowIDs []string, response commoncap.CapabilityResponse) error {
	marshaled, err := pb.MarshalCapabilityResponse(response)
	if err != nil {
		return err
	}
	msg := &types.MessageBody{
		CapabilityId:    p.capInfo.ID,
		CapabilityDonId: p.capDonInfo.ID,
		CallerDonId:     destDonID,
		Method:          types.MethodTriggerEvent,
		Payload:         marshaled,
		Metadata: &types.MessageBody_TriggerEventMetadata{
			TriggerEventMetadata: &types.TriggerEventMetadata{
				WorkflowIds: destWorkflowIDs,
			},
		},
	}
	// NOTE: send to all by default, introduce different strategies later (KS-76)
	err = nil
	for _, peerID := range p.workflowDONs[destDonID].Members {
		err = errors.Join(err, p.dispatcher.Send(peerID, msg))
	}
	return err
}

func (p *triggerPublisher) Close() error {
	close(p.stopCh)
	p.wg.Wait()
	p.lggr.Info("TriggerPublisher closed")
	return nil
}

func (p *triggerPublisher) Ready() error {
	return nil
}

func (p *triggerPublisher) HealthReport() map[string]error {
	return nil
}

func (p *triggerPublisher) Name() string {
	return "TriggerPublisher"
}
