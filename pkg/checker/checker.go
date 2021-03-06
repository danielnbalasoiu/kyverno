package checker

import (
	"sync"
	"time"

	"github.com/go-logr/logr"
	kyvernolister "github.com/nirmata/kyverno/pkg/client/listers/kyverno/v1"
	dclient "github.com/nirmata/kyverno/pkg/dclient"
	"github.com/nirmata/kyverno/pkg/event"
	"k8s.io/apimachinery/pkg/labels"
)

//MaxRetryCount defines the max deadline count
const (
	MaxRetryCount   int           = 3
	DefaultDeadline time.Duration = 60 * time.Second
	DefaultResync   time.Duration = 60 * time.Second
)

// LastReqTime stores the lastrequest times for incoming api-requests
type LastReqTime struct {
	t   time.Time
	mu  sync.RWMutex
	log logr.Logger
}

//Time returns the lastrequest time
func (t *LastReqTime) Time() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.t
}

//SetTime stes the lastrequest time
func (t *LastReqTime) SetTime(tm time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.t = tm
}

//NewLastReqTime returns a new instance of LastRequestTime store
func NewLastReqTime(log logr.Logger) *LastReqTime {
	return &LastReqTime{
		t:   time.Now(),
		log: log,
	}
}

func checkIfPolicyWithMutateAndGenerateExists(pLister kyvernolister.ClusterPolicyLister, log logr.Logger) bool {
	policies, err := pLister.ListResources(labels.NewSelector())
	if err != nil {
		log.Error(err, "failed to list cluster policies")
	}
	for _, policy := range policies {
		if policy.HasMutateOrValidateOrGenerate() {
			// as there exists one policy with mutate or validate rule
			// so there must be a webhook configuration on resource
			return true
		}
	}
	return false
}

//Run runs the checker and verify the resource update
func (t *LastReqTime) Run(pLister kyvernolister.ClusterPolicyLister, eventGen event.Interface, client *dclient.Client, defaultResync time.Duration, deadline time.Duration, stopCh <-chan struct{}) {
	logger := t.log
	logger.V(2).Info("tarting default resync for webhook checker", "resyncTime", defaultResync)
	maxDeadline := deadline * time.Duration(MaxRetryCount)
	ticker := time.NewTicker(defaultResync)
	/// interface to update and increment kyverno webhook status via annotations
	statuscontrol := NewVerifyControl(client, eventGen, logger.WithName("StatusControl"))
	// send the initial update status
	if checkIfPolicyWithMutateAndGenerateExists(pLister, logger) {
		if err := statuscontrol.SuccessStatus(); err != nil {
			logger.Error(err, "failed to set 'success' status")
		}
	}

	defer ticker.Stop()
	// - has received request ->  set webhookstatus as "True"
	// - no requests received
	// 						  -> if greater than deadline, send update request
	// 						  -> if greater than maxDeadline, send failed status update
	for {
		select {
		case <-ticker.C:
			// if there are no policies then we dont have a webhook on resource.
			// we indirectly check if the resource
			if !checkIfPolicyWithMutateAndGenerateExists(pLister, logger) {
				continue
			}
			// get current time
			timeDiff := time.Since(t.Time())
			if timeDiff > maxDeadline {
				logger.Info("request exceeded max deadline", "deadline", maxDeadline)
				logger.Info("Admission Control failing: Webhook is not receiving requests forwarded by api-server as per webhook configurations")
				// set the status unavailable
				if err := statuscontrol.FailedStatus(); err != nil {
					logger.Error(err, "failed to set 'failed' status")
				}
				continue
			}
			if timeDiff > deadline {
				logger.Info("Admission Control failing: Webhook is not receiving requests forwarded by api-server as per webhook configurations")
				// send request to update the kyverno deployment
				if err := statuscontrol.IncrementAnnotation(); err != nil {
					logger.Error(err, "failed to increment annotation")
				}
				continue
			}
			// if the status was false before then we update it to true
			// send request to update the kyverno deployment
			if err := statuscontrol.SuccessStatus(); err != nil {
				logger.Error(err, "failed to update success status")
			}
		case <-stopCh:
			// handler termination signal
			logger.V(2).Info("stopping default resync for webhook checker")
			return
		}
	}
}
