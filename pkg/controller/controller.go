package controller

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/minio/minio/pkg/wildcard"
	types "github.com/nirmata/kyverno/pkg/apis/policy/v1alpha1"
	lister "github.com/nirmata/kyverno/pkg/client/listers/policy/v1alpha1"
	client "github.com/nirmata/kyverno/pkg/dclient"
	"github.com/nirmata/kyverno/pkg/engine"
	"github.com/nirmata/kyverno/pkg/event"
	"github.com/nirmata/kyverno/pkg/sharedinformer"
	violation "github.com/nirmata/kyverno/pkg/violation"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

//PolicyController to manage Policy CRD
type PolicyController struct {
	client           *client.Client
	policyLister     lister.PolicyLister
	policySynced     cache.InformerSynced
	violationBuilder violation.Generator
	eventBuilder     event.Generator
	queue            workqueue.RateLimitingInterface
}

// NewPolicyController from cmd args
func NewPolicyController(client *client.Client,
	policyInformer sharedinformer.PolicyInformer,
	violationBuilder violation.Generator,
	eventController event.Generator) *PolicyController {

	controller := &PolicyController{
		client:           client,
		policyLister:     policyInformer.GetLister(),
		policySynced:     policyInformer.GetInfomer().HasSynced,
		violationBuilder: violationBuilder,
		eventBuilder:     eventController,
		queue:            workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), policyWorkQueueName),
	}

	policyInformer.GetInfomer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.createPolicyHandler,
		UpdateFunc: controller.updatePolicyHandler,
		DeleteFunc: controller.deletePolicyHandler,
	})
	return controller
}

func (pc *PolicyController) createPolicyHandler(resource interface{}) {
	pc.enqueuePolicy(resource)
}

func (pc *PolicyController) updatePolicyHandler(oldResource, newResource interface{}) {
	newPolicy := newResource.(*types.Policy)
	oldPolicy := oldResource.(*types.Policy)
	if newPolicy.ResourceVersion == oldPolicy.ResourceVersion {
		return
	}
	pc.enqueuePolicy(newResource)
}

func (pc *PolicyController) deletePolicyHandler(resource interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = resource.(metav1.Object); !ok {
		utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
		return
	}
	glog.Infof("policy deleted: %s", object.GetName())
}

func (pc *PolicyController) enqueuePolicy(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	pc.queue.Add(key)
}

// Run is main controller thread
func (pc *PolicyController) Run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer pc.queue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, pc.policySynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	for i := 0; i < policyControllerWorkerCount; i++ {
		go wait.Until(pc.runWorker, time.Second, stopCh)
	}
	glog.Info("started policy controller workers")

	return nil
}

//Stop to perform actions when controller is stopped
func (pc *PolicyController) Stop() {
	glog.Info("shutting down policy controller workers")
}
func (pc *PolicyController) runWorker() {
	for pc.processNextWorkItem() {
	}
}

func (pc *PolicyController) processNextWorkItem() bool {
	obj, shutdown := pc.queue.Get()
	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer pc.queue.Done(obj)
		err := pc.syncHandler(obj)
		pc.handleErr(err, obj)
		return nil
	}(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return true
	}
	return true
}

func (pc *PolicyController) handleErr(err error, key interface{}) {
	if err == nil {
		pc.queue.Forget(key)
		return
	}
	// This controller retries if something goes wrong. After that, it stops trying.
	if pc.queue.NumRequeues(key) < policyWorkQueueRetryLimit {
		glog.Warningf("Error syncing events %v: %v", key, err)
		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		pc.queue.AddRateLimited(key)
		return
	}
	pc.queue.Forget(key)
	utilruntime.HandleError(err)
	glog.Warningf("Dropping the key %q out of the queue: %v", key, err)
}

func (pc *PolicyController) syncHandler(obj interface{}) error {
	var key string
	var ok bool
	if key, ok = obj.(string); !ok {
		return fmt.Errorf("expected string in workqueue but got %#v", obj)
	}
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("invalid policy key: %s", key))
		return nil
	}

	// Get Policy resource with namespace/name
	policy, err := pc.policyLister.Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("policy '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}
	// process policy on existing resource
	// get the violations and pass to violation Builder
	// get the events and pass to event Builder
	//TODO: processPolicy
	pc.processPolicy(policy)

	glog.Infof("process policy %s on existing resources", policy.GetName())
	return nil
}

func (pc *PolicyController) processPolicy(p *types.Policy) {
	// Get all resources on which the policy is to be applied
	resources := []*resourceInfo{}
	for _, rule := range p.Spec.Rules {
		for _, k := range rule.Kinds {
			// get resources of defined kinds->resources
			gvr := pc.client.GetGVRFromKind(k)
			// LabelSelector
			// namespace ?
			list, err := pc.client.ListResource(gvr.Resource, "", rule.ResourceDescription.Selector)
			if err != nil {
				glog.Errorf("unable to list resources for %s with label selector %s", gvr.Resource, rule.Selector.String())
				glog.Errorf("unable to apply policy %s rule %s. err : %s", p.Name, rule.Name, err)
				continue
			}

			for _, resource := range list.Items {
				name := rule.ResourceDescription.Name
				gvk := resource.GroupVersionKind()
				rawResource, err := resource.MarshalJSON()
				if err != nil {
					glog.Errorf("Unable to json parse resource %s", resource.GetName())
					continue
				}
				if name != nil {
					// wild card matching
					if !wildcard.Match(*name, resource.GetName()) {
						continue
					}
				}
				ri := &resourceInfo{rawResource: rawResource, gvk: &metav1.GroupVersionKind{Group: gvk.Group,
					Version: gvk.Version,
					Kind:    gvk.Kind}}
				resources = append(resources, ri)
			}
		}
	}
	// for the filtered resource apply policy
	for _, r := range resources {
		pc.applyPolicy(p, r.rawResource, r.gvk)
	}
	// apply policies on the filtered resources
}

func (pc *PolicyController) applyPolicy(p *types.Policy, rawResource []byte, gvk *metav1.GroupVersionKind) {
	//TODO: PR #181 use the list of kinds to filter here too

	patches, result := engine.Mutate(*p, rawResource, *gvk)
	//	err := result.ToError()
	fmt.Println(result.String())
	// create events accordingly to result
	if patches != nil {
		// patches should be nil or empty if the overlay or patch is already applied
		// as the existing resouces are not to be modified we create policy violations
		// Create Violation
	}
	result = engine.Validate(*p, rawResource, *gvk)
	fmt.Println(result.String())
	// create events accordingly to result
	// Generate ??
}
