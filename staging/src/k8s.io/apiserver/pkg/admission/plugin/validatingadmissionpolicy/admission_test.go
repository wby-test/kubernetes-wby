/*
Copyright 2022 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package validatingadmissionpolicy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/api/admissionregistration/v1alpha1"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/apiserver/pkg/admission/plugin/validatingadmissionpolicy/internal/generic"
	whgeneric "k8s.io/apiserver/pkg/admission/plugin/webhook/generic"
	"k8s.io/apiserver/pkg/features"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/component-base/featuregate"
	"k8s.io/klog/v2"
)

var (
	scheme *runtime.Scheme = func() *runtime.Scheme {
		res := runtime.NewScheme()
		res.AddKnownTypeWithName(paramsGVK, &unstructured.Unstructured{})
		res.AddKnownTypeWithName(schema.GroupVersionKind{
			Group:   paramsGVK.Group,
			Version: paramsGVK.Version,
			Kind:    paramsGVK.Kind + "List",
		}, &unstructured.UnstructuredList{})

		if err := v1alpha1.AddToScheme(res); err != nil {
			panic(err)
		}

		if err := fake.AddToScheme(res); err != nil {
			panic(err)
		}

		return res
	}()
	paramsGVK schema.GroupVersionKind = schema.GroupVersionKind{
		Group:   "example.com",
		Version: "v1",
		Kind:    "ParamsConfig",
	}

	fakeRestMapper *meta.DefaultRESTMapper = func() *meta.DefaultRESTMapper {
		res := meta.NewDefaultRESTMapper([]schema.GroupVersion{
			{
				Group:   "",
				Version: "v1",
			},
		})

		res.Add(paramsGVK, meta.RESTScopeNamespace)
		res.Add(definitionGVK, meta.RESTScopeRoot)
		res.Add(bindingGVK, meta.RESTScopeRoot)
		res.Add(v1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
		return res
	}()

	definitionGVK schema.GroupVersionKind = must3(scheme.ObjectKinds(&v1alpha1.ValidatingAdmissionPolicy{}))[0]
	bindingGVK    schema.GroupVersionKind = must3(scheme.ObjectKinds(&v1alpha1.ValidatingAdmissionPolicyBinding{}))[0]

	definitionsGVR schema.GroupVersionResource = must(fakeRestMapper.RESTMapping(definitionGVK.GroupKind(), definitionGVK.Version)).Resource
	bindingsGVR    schema.GroupVersionResource = must(fakeRestMapper.RESTMapping(bindingGVK.GroupKind(), bindingGVK.Version)).Resource

	// Common objects
	denyPolicy *v1alpha1.ValidatingAdmissionPolicy = &v1alpha1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "denypolicy.example.com",
			ResourceVersion: "1",
		},
		Spec: v1alpha1.ValidatingAdmissionPolicySpec{
			ParamKind: &v1alpha1.ParamKind{
				APIVersion: paramsGVK.GroupVersion().String(),
				Kind:       paramsGVK.Kind,
			},
			FailurePolicy: ptrTo(v1alpha1.Fail),
		},
	}

	fakeParams *unstructured.Unstructured = &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": paramsGVK.GroupVersion().String(),
			"kind":       paramsGVK.Kind,
			"metadata": map[string]interface{}{
				"name":            "replicas-test.example.com",
				"resourceVersion": "1",
			},
			"maxReplicas": int64(3),
		},
	}

	denyBinding *v1alpha1.ValidatingAdmissionPolicyBinding = &v1alpha1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "denybinding.example.com",
			ResourceVersion: "1",
		},
		Spec: v1alpha1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: denyPolicy.Name,
			ParamRef: &v1alpha1.ParamRef{
				Name:      fakeParams.GetName(),
				Namespace: fakeParams.GetNamespace(),
			},
		},
	}
	denyBindingWithNoParamRef *v1alpha1.ValidatingAdmissionPolicyBinding = &v1alpha1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "denybinding.example.com",
			ResourceVersion: "1",
		},
		Spec: v1alpha1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: denyPolicy.Name,
		},
	}
)

// Interface which has fake compile and match functionality for use in tests
// So that we can test the controller without pulling in any CEL functionality
type fakeCompiler struct {
	DefaultMatch         bool
	CompileFuncs         map[namespacedName]func(*v1alpha1.ValidatingAdmissionPolicy) Validator
	DefinitionMatchFuncs map[namespacedName]func(*v1alpha1.ValidatingAdmissionPolicy, admission.Attributes) bool
	BindingMatchFuncs    map[namespacedName]func(*v1alpha1.ValidatingAdmissionPolicyBinding, admission.Attributes) bool
}

var _ ValidatorCompiler = &fakeCompiler{}

func (f *fakeCompiler) HasSynced() bool {
	return true
}

func (f *fakeCompiler) ValidateInitialization() error {
	return nil
}

// Matches says whether this policy definition matches the provided admission
// resource request
func (f *fakeCompiler) DefinitionMatches(a admission.Attributes, o admission.ObjectInterfaces, definition *v1alpha1.ValidatingAdmissionPolicy) (bool, schema.GroupVersionKind, error) {
	namespace, name := definition.Namespace, definition.Name
	key := namespacedName{
		name:      name,
		namespace: namespace,
	}
	if fun, ok := f.DefinitionMatchFuncs[key]; ok {
		return fun(definition, a), a.GetKind(), nil
	}

	// Default is match everything
	return f.DefaultMatch, a.GetKind(), nil
}

// Matches says whether this policy definition matches the provided admission
// resource request
func (f *fakeCompiler) BindingMatches(a admission.Attributes, o admission.ObjectInterfaces, binding *v1alpha1.ValidatingAdmissionPolicyBinding) (bool, error) {
	namespace, name := binding.Namespace, binding.Name
	key := namespacedName{
		name:      name,
		namespace: namespace,
	}
	if fun, ok := f.BindingMatchFuncs[key]; ok {
		return fun(binding, a), nil
	}

	// Default is match everything
	return f.DefaultMatch, nil
}

func (f *fakeCompiler) Compile(
	definition *v1alpha1.ValidatingAdmissionPolicy,
) Validator {
	namespace, name := definition.Namespace, definition.Name
	key := namespacedName{
		name:      name,
		namespace: namespace,
	}
	if fun, ok := f.CompileFuncs[key]; ok {
		return fun(definition)
	}

	return nil
}

func (f *fakeCompiler) RegisterDefinition(definition *v1alpha1.ValidatingAdmissionPolicy, compileFunc func(*v1alpha1.ValidatingAdmissionPolicy) Validator, matchFunc func(*v1alpha1.ValidatingAdmissionPolicy, admission.Attributes) bool) {
	namespace, name := definition.Namespace, definition.Name
	key := namespacedName{
		name:      name,
		namespace: namespace,
	}
	if compileFunc != nil {

		if f.CompileFuncs == nil {
			f.CompileFuncs = make(map[namespacedName]func(*v1alpha1.ValidatingAdmissionPolicy) Validator)
		}
		f.CompileFuncs[key] = compileFunc
	}

	if matchFunc != nil {
		if f.DefinitionMatchFuncs == nil {
			f.DefinitionMatchFuncs = make(map[namespacedName]func(*v1alpha1.ValidatingAdmissionPolicy, admission.Attributes) bool)
		}
		f.DefinitionMatchFuncs[key] = matchFunc
	}
}

func (f *fakeCompiler) RegisterBinding(binding *v1alpha1.ValidatingAdmissionPolicyBinding, matchFunc func(*v1alpha1.ValidatingAdmissionPolicyBinding, admission.Attributes) bool) {
	namespace, name := binding.Namespace, binding.Name
	key := namespacedName{
		name:      name,
		namespace: namespace,
	}

	if matchFunc != nil {
		if f.BindingMatchFuncs == nil {
			f.BindingMatchFuncs = make(map[namespacedName]func(*v1alpha1.ValidatingAdmissionPolicyBinding, admission.Attributes) bool)
		}
		f.BindingMatchFuncs[key] = matchFunc
	}
}

func setupFakeTest(t *testing.T, comp *fakeCompiler) (plugin admission.ValidationInterface, paramTracker, policyTracker clienttesting.ObjectTracker, controller *celAdmissionController) {
	return setupTestCommon(t, comp, true)
}

// Starts CEL admission controller and sets up a plugin configured with it as well
// as object trackers for manipulating the objects available to the system
//
// ParamTracker only knows the gvk `paramGVK`. If in the future we need to
// support multiple types of params this function needs to be augmented
//
// PolicyTracker expects FakePolicyDefinition and FakePolicyBinding types
// !TODO: refactor this test/framework to remove startInformers argument and
// clean up the return args, and in general make it more accessible.
func setupTestCommon(t *testing.T, compiler ValidatorCompiler, shouldStartInformers bool) (plugin admission.ValidationInterface, paramTracker, policyTracker clienttesting.ObjectTracker, controller *celAdmissionController) {
	testContext, testContextCancel := context.WithCancel(context.Background())
	t.Cleanup(testContextCancel)

	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme)

	fakeClient := fake.NewSimpleClientset()
	fakeInformerFactory := informers.NewSharedInformerFactory(fakeClient, time.Second)
	featureGate := featuregate.NewFeatureGate()
	err := featureGate.Add(map[featuregate.Feature]featuregate.FeatureSpec{
		features.ValidatingAdmissionPolicy: {
			Default: true, PreRelease: featuregate.Alpha}})
	if err != nil {
		t.Fatalf("Unable to add feature gate: %v", err)
	}
	err = featureGate.SetFromMap(map[string]bool{string(features.ValidatingAdmissionPolicy): true})
	if err != nil {
		t.Fatalf("Unable to store flag gate: %v", err)
	}

	plug, err := NewPlugin()
	require.NoError(t, err)

	handler := plug.(*celAdmissionPlugin)
	handler.enabled = true

	genericInitializer := initializer.New(fakeClient, dynamicClient, fakeInformerFactory, nil, featureGate, testContext.Done())
	genericInitializer.Initialize(handler)
	handler.SetRESTMapper(fakeRestMapper)
	err = admission.ValidateInitialization(handler)
	require.NoError(t, err)
	require.True(t, handler.enabled)

	// Override compiler used by controller for tests
	controller = handler.evaluator.(*celAdmissionController)
	controller.policyController.ValidatorCompiler = compiler

	t.Cleanup(func() {
		testContextCancel()
		// wait for informer factory to shutdown
		fakeInformerFactory.Shutdown()
	})

	if !shouldStartInformers {
		return handler, dynamicClient.Tracker(), fakeClient.Tracker(), controller
	}

	// Make sure to start the fake informers
	fakeInformerFactory.Start(testContext.Done())

	// Wait for admission controller to begin its object watches
	// This is because there is a very rare (0.05% on my machine) race doing the
	// initial List+Watch if an object is added after the list, but before the
	// watch it could be missed.
	//
	// This is only due to the fact that NewSimpleClientset above ignores
	// LastSyncResourceVersion on watch calls, so do it does not provide "catch up"
	// which may have been added since the call to list.
	if !cache.WaitForNamedCacheSync("initial sync", testContext.Done(), handler.evaluator.HasSynced) {
		t.Fatal("failed to do perform initial cache sync")
	}

	// WaitForCacheSync only tells us the list was performed.
	// Keep changing an object until it is observable, then remove it

	i := 0

	dummyPolicy := &v1alpha1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dummypolicy.example.com",
			Annotations: map[string]string{
				"myValue": fmt.Sprint(i),
			},
		},
	}

	dummyBinding := &v1alpha1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dummybinding.example.com",
			Annotations: map[string]string{
				"myValue": fmt.Sprint(i),
			},
		},
	}

	require.NoError(t, fakeClient.Tracker().Create(definitionsGVR, dummyPolicy, dummyPolicy.Namespace))
	require.NoError(t, fakeClient.Tracker().Create(bindingsGVR, dummyBinding, dummyBinding.Namespace))

	wait.PollWithContext(testContext, 100*time.Millisecond, 300*time.Millisecond, func(ctx context.Context) (done bool, err error) {
		defer func() {
			i += 1
		}()

		dummyPolicy.Annotations = map[string]string{
			"myValue": fmt.Sprint(i),
		}
		dummyBinding.Annotations = dummyPolicy.Annotations

		require.NoError(t, fakeClient.Tracker().Update(definitionsGVR, dummyPolicy, dummyPolicy.Namespace))
		require.NoError(t, fakeClient.Tracker().Update(bindingsGVR, dummyBinding, dummyBinding.Namespace))

		if obj, err := controller.getCurrentObject(dummyPolicy); obj == nil || err != nil {
			return false, nil
		}

		if obj, err := controller.getCurrentObject(dummyBinding); obj == nil || err != nil {
			return false, nil
		}

		return true, nil
	})

	require.NoError(t, fakeClient.Tracker().Delete(definitionsGVR, dummyPolicy.Namespace, dummyPolicy.Name))
	require.NoError(t, fakeClient.Tracker().Delete(bindingsGVR, dummyBinding.Namespace, dummyBinding.Name))

	return handler, dynamicClient.Tracker(), fakeClient.Tracker(), controller
}

// Gets the last reconciled value in the controller of an object with the same
// gvk and name as the given object
//
// If the object is not found both the error and object will be nil.
func (c *celAdmissionController) getCurrentObject(obj runtime.Object) (runtime.Object, error) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}

	c.policyController.mutex.RLock()
	defer c.policyController.mutex.RUnlock()

	switch obj.(type) {
	case *v1alpha1.ValidatingAdmissionPolicyBinding:
		nn := getNamespaceName(accessor.GetNamespace(), accessor.GetName())
		info, ok := c.policyController.bindingInfos[nn]
		if !ok {
			return nil, nil
		}

		return info.lastReconciledValue, nil
	case *v1alpha1.ValidatingAdmissionPolicy:
		nn := getNamespaceName(accessor.GetNamespace(), accessor.GetName())
		info, ok := c.policyController.definitionInfo[nn]
		if !ok {
			return nil, nil
		}

		return info.lastReconciledValue, nil
	default:
		// If test isn't trying to fetch a policy or binding, assume it is
		// fetching a param
		paramSourceGVK := obj.GetObjectKind().GroupVersionKind()
		paramKind := v1alpha1.ParamKind{
			APIVersion: paramSourceGVK.GroupVersion().String(),
			Kind:       paramSourceGVK.Kind,
		}

		var paramInformer generic.Informer[runtime.Object]
		if paramInfo, ok := c.policyController.paramsCRDControllers[paramKind]; ok {
			paramInformer = paramInfo.controller.Informer()
		} else {
			// Treat unknown CRD the same as not found
			return nil, nil
		}

		// Param type. Just check informer for its GVK
		item, err := paramInformer.Get(accessor.GetName())
		if err != nil {
			if k8serrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}

		return item, nil
	}
}

// Waits for the given objects to have been the latest reconciled values of
// their gvk/name in the controller
func waitForReconcile(ctx context.Context, controller *celAdmissionController, objects ...runtime.Object) error {
	return wait.PollWithContext(ctx, 100*time.Millisecond, 1*time.Second, func(ctx context.Context) (done bool, err error) {
		defer func() {
			if done {
				// force admission controller to refresh the information it
				// uses for validation now that it is done in the background
				controller.refreshPolicies()
			}
		}()
		for _, obj := range objects {

			objMeta, err := meta.Accessor(obj)
			if err != nil {
				return false, fmt.Errorf("error getting meta accessor for original %T object (%v): %w", obj, obj, err)
			}

			currentValue, err := controller.getCurrentObject(obj)
			if err != nil {
				return false, fmt.Errorf("error getting current object: %w", err)
			} else if currentValue == nil {
				// Object not found, but not an error. Keep waiting.
				klog.Infof("%v not found. keep waiting", objMeta.GetName())
				return false, nil
			}

			valueMeta, err := meta.Accessor(currentValue)
			if err != nil {
				return false, fmt.Errorf("error getting meta accessor for current %T object (%v): %w", currentValue, currentValue, err)
			}

			if len(objMeta.GetResourceVersion()) == 0 {
				return false, fmt.Errorf("%s named %s has no resource version. please ensure your test objects have an RV",
					obj.GetObjectKind().GroupVersionKind().String(), objMeta.GetName())
			} else if len(valueMeta.GetResourceVersion()) == 0 {
				return false, fmt.Errorf("%s named %s has no resource version. please ensure your test objects have an RV",
					currentValue.GetObjectKind().GroupVersionKind().String(), valueMeta.GetName())
			} else if objMeta.GetResourceVersion() != valueMeta.GetResourceVersion() {
				klog.Infof("%v has RV %v. want RV %v", objMeta.GetName(), objMeta.GetResourceVersion(), objMeta.GetResourceVersion())
				return false, nil
			}
		}

		return true, nil
	})
}

// Waits for the admissoin controller to have no knowledge of the objects
// with the given GVKs and namespace/names
func waitForReconcileDeletion(ctx context.Context, controller *celAdmissionController, objects ...runtime.Object) error {
	return wait.PollWithContext(ctx, 200*time.Millisecond, 3*time.Hour, func(ctx context.Context) (done bool, err error) {
		defer func() {
			if done {
				// force admission controller to refresh the information it
				// uses for validation now that it is done in the background
				controller.refreshPolicies()
			}
		}()

		for _, obj := range objects {
			currentValue, err := controller.getCurrentObject(obj)
			if err != nil {
				return false, err
			}

			if currentValue != nil {
				return false, nil
			}
		}

		return true, nil
	})
}

func attributeRecord(
	old, new runtime.Object,
	operation admission.Operation,
) admission.Attributes {
	if old == nil && new == nil {
		panic("both `old` and `new` may not be nil")
	}

	accessor, err := meta.Accessor(new)
	if err != nil {
		panic(err)
	}

	// one of old/new may be nil, but not both
	example := new
	if example == nil {
		example = old
	}

	gvk := example.GetObjectKind().GroupVersionKind()
	if gvk.Empty() {
		// If gvk is not populated, try to fetch it from the scheme
		gvk = must3(scheme.ObjectKinds(example))[0]
	}
	mapping, err := fakeRestMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		panic(err)
	}

	return admission.NewAttributesRecord(
		new,
		old,
		gvk,
		accessor.GetNamespace(),
		accessor.GetName(),
		mapping.Resource,
		"",
		operation,
		nil,
		false,
		nil,
	)
}

func ptrTo[T any](obj T) *T {
	return &obj
}

func must[T any](val T, err error) T {
	if err != nil {
		panic(err)
	}
	return val
}

func must3[T any, I any](val T, _ I, err error) T {
	if err != nil {
		panic(err)
	}
	return val
}

////////////////////////////////////////////////////////////////////////////////
// Functionality Tests
////////////////////////////////////////////////////////////////////////////////

func TestPluginNotReady(t *testing.T) {
	compiler := &fakeCompiler{
		// Match everything by default
		DefaultMatch: true,
	}

	// Show that an unstarted informer (or one that has failed its listwatch)
	// will show proper error from plugin
	handler, _, _, _ := setupTestCommon(t, compiler, false)
	err := handler.Validate(
		context.Background(),
		// Object is irrelevant/unchecked for this test. Just test that
		// the evaluator is executed, and returns a denial
		attributeRecord(nil, fakeParams, admission.Create),
		&admission.RuntimeObjectInterfaces{},
	)

	require.ErrorContains(t, err, "not yet ready to handle request")

	// Show that by now starting the informer, the error is dissipated
	handler, _, _, _ = setupTestCommon(t, compiler, true)
	err = handler.Validate(
		context.Background(),
		// Object is irrelevant/unchecked for this test. Just test that
		// the evaluator is executed, and returns a denial
		attributeRecord(nil, fakeParams, admission.Create),
		&admission.RuntimeObjectInterfaces{},
	)

	require.NoError(t, err)
}

func TestBasicPolicyDefinitionFailure(t *testing.T) {
	testContext, testContextCancel := context.WithCancel(context.Background())
	defer testContextCancel()

	datalock := sync.Mutex{}
	numCompiles := 0

	compiler := &fakeCompiler{
		// Match everything by default
		DefaultMatch: true,
	}
	compiler.RegisterDefinition(denyPolicy, func(policy *v1alpha1.ValidatingAdmissionPolicy) Validator {
		datalock.Lock()
		numCompiles += 1
		datalock.Unlock()

		return testValidator{}
	}, nil)

	handler, paramTracker, tracker, controller := setupFakeTest(t, compiler)

	require.NoError(t, paramTracker.Add(fakeParams))
	require.NoError(t, tracker.Create(definitionsGVR, denyPolicy, denyPolicy.Namespace))
	require.NoError(t, tracker.Create(bindingsGVR, denyBinding, denyBinding.Namespace))

	// Wait for controller to reconcile given objects
	require.NoError(t,
		waitForReconcile(
			testContext, controller,
			fakeParams, denyBinding, denyPolicy))

	err := handler.Validate(
		testContext,
		// Object is irrelevant/unchecked for this test. Just test that
		// the evaluator is executed, and returns a denial
		attributeRecord(nil, fakeParams, admission.Create),
		&admission.RuntimeObjectInterfaces{},
	)

	require.ErrorContains(t, err, `Denied`)
}

type validatorFunc func(versionedAttr *whgeneric.VersionedAttributes, versionedParams runtime.Object) ([]PolicyDecision, error)

func (f validatorFunc) Validate(versionedAttr *whgeneric.VersionedAttributes, versionedParams runtime.Object) ([]PolicyDecision, error) {
	return f(versionedAttr, versionedParams)
}

type testValidator struct {
}

func (v testValidator) Validate(versionedAttr *whgeneric.VersionedAttributes, versionedParams runtime.Object) ([]PolicyDecision, error) {
	// Policy always denies
	return []PolicyDecision{
		{
			Action:  ActionDeny,
			Message: "Denied",
		},
	}, nil
}

// Shows that if a definition does not match the input, it will not be used.
// But with a different input it will be used.
func TestDefinitionDoesntMatch(t *testing.T) {
	compiler := &fakeCompiler{
		// Match everything by default
		DefaultMatch: true,
	}
	handler, paramTracker, tracker, controller := setupFakeTest(t, compiler)
	testContext, testContextCancel := context.WithCancel(context.Background())
	defer testContextCancel()

	datalock := sync.Mutex{}
	passedParams := []*unstructured.Unstructured{}
	numCompiles := 0

	compiler.RegisterDefinition(denyPolicy,
		func(vap *v1alpha1.ValidatingAdmissionPolicy) Validator {
			datalock.Lock()
			numCompiles += 1
			datalock.Unlock()

			return testValidator{}

		}, func(vap *v1alpha1.ValidatingAdmissionPolicy, a admission.Attributes) bool {
			// Match names with even-numbered length
			obj := a.GetObject()

			accessor, err := meta.Accessor(obj)
			if err != nil {
				t.Fatal(err)
				return false
			}

			return len(accessor.GetName())%2 == 0
		})

	require.NoError(t, paramTracker.Add(fakeParams))
	require.NoError(t, tracker.Create(definitionsGVR, denyPolicy, denyPolicy.Namespace))
	require.NoError(t, tracker.Create(bindingsGVR, denyBinding, denyBinding.Namespace))

	// Wait for controller to reconcile given objects
	require.NoError(t,
		waitForReconcile(
			testContext, controller,
			fakeParams, denyBinding, denyPolicy))

	// Validate a non-matching input.
	// Should pass validation with no error.

	nonMatchingParams := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": paramsGVK.GroupVersion().String(),
			"kind":       paramsGVK.Kind,
			"metadata": map[string]interface{}{
				"name":            "oddlength",
				"resourceVersion": "1",
			},
		},
	}
	require.NoError(t,
		handler.Validate(testContext,
			attributeRecord(
				nil, nonMatchingParams,
				admission.Create), &admission.RuntimeObjectInterfaces{}))
	require.Empty(t, passedParams)

	// Validate a matching input.
	// Should match and be denied.
	matchingParams := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": paramsGVK.GroupVersion().String(),
			"kind":       paramsGVK.Kind,
			"metadata": map[string]interface{}{
				"name":            "evenlength",
				"resourceVersion": "1",
			},
		},
	}
	require.ErrorContains(t,
		handler.Validate(testContext,
			attributeRecord(
				nil, matchingParams,
				admission.Create), &admission.RuntimeObjectInterfaces{}),
		`Denied`)
	require.Equal(t, numCompiles, 1)
}

func TestReconfigureBinding(t *testing.T) {
	testContext, testContextCancel := context.WithCancel(context.Background())
	defer testContextCancel()

	compiler := &fakeCompiler{
		// Match everything by default
		DefaultMatch: true,
	}

	handler, paramTracker, tracker, controller := setupFakeTest(t, compiler)

	datalock := sync.Mutex{}
	numCompiles := 0

	fakeParams2 := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": paramsGVK.GroupVersion().String(),
			"kind":       paramsGVK.Kind,
			"metadata": map[string]interface{}{
				"name":            "replicas-test2.example.com",
				"resourceVersion": "2",
			},
			"maxReplicas": int64(35),
		},
	}

	compiler.RegisterDefinition(denyPolicy,
		func(vap *v1alpha1.ValidatingAdmissionPolicy) Validator {
			datalock.Lock()
			numCompiles += 1
			datalock.Unlock()

			return testValidator{}

		}, nil)

	denyBinding2 := &v1alpha1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "denybinding.example.com",
			ResourceVersion: "2",
		},
		Spec: v1alpha1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName: denyPolicy.Name,
			ParamRef: &v1alpha1.ParamRef{
				Name:      fakeParams2.GetName(),
				Namespace: fakeParams2.GetNamespace(),
			},
		},
	}

	require.NoError(t, paramTracker.Add(fakeParams))
	require.NoError(t, tracker.Create(definitionsGVR, denyPolicy, denyPolicy.Namespace))
	require.NoError(t, tracker.Create(bindingsGVR, denyBinding, denyBinding.Namespace))

	// Wait for controller to reconcile given objects
	require.NoError(t,
		waitForReconcile(
			testContext, controller,
			fakeParams, denyBinding, denyPolicy))

	err := handler.Validate(
		testContext,
		attributeRecord(nil, fakeParams, admission.Create),
		&admission.RuntimeObjectInterfaces{},
	)

	// Expect validation to fail for first time due to binding unconditionally
	// failing
	require.ErrorContains(t, err, `Denied`, "expect policy validation error")

	// Expect `Compile` only called once
	require.Equal(t, 1, numCompiles, "expect `Compile` to be called only once")

	// Update the tracker to point at different params
	require.NoError(t, tracker.Update(bindingsGVR, denyBinding2, ""))

	// Wait for update to propagate
	// Wait for controller to reconcile given objects
	require.NoError(t, waitForReconcile(testContext, controller, denyBinding2))

	err = handler.Validate(
		testContext,
		attributeRecord(nil, fakeParams, admission.Create),
		&admission.RuntimeObjectInterfaces{},
	)

	require.ErrorContains(t, err, `failed to configure binding: replicas-test2.example.com not found`)

	// Add the missing params
	require.NoError(t, paramTracker.Add(fakeParams2))

	// Wait for update to propagate
	require.NoError(t, waitForReconcile(testContext, controller, fakeParams2))

	// Expect validation to now fail again.
	err = handler.Validate(
		testContext,
		attributeRecord(nil, fakeParams, admission.Create),
		&admission.RuntimeObjectInterfaces{},
	)

	// Expect validation to fail the third time due to validation failure
	require.ErrorContains(t, err, `Denied`, "expected a true policy failure, not a configuration error")
	//require.Equal(t, []*unstructured.Unstructured{fakeParams, fakeParams2}, passedParams, "expected call to `Validate` to cause call to evaluator")
	require.Equal(t, 2, numCompiles, "expect changing binding causes a recompile")
}

// Shows that a policy which is in effect will stop being in effect when removed
func TestRemoveDefinition(t *testing.T) {
	testContext, testContextCancel := context.WithCancel(context.Background())
	defer testContextCancel()

	compiler := &fakeCompiler{
		// Match everything by default
		DefaultMatch: true,
	}
	handler, paramTracker, tracker, controller := setupFakeTest(t, compiler)

	datalock := sync.Mutex{}
	numCompiles := 0

	compiler.RegisterDefinition(denyPolicy, func(vap *v1alpha1.ValidatingAdmissionPolicy) Validator {
		datalock.Lock()
		numCompiles += 1
		datalock.Unlock()

		return testValidator{}
	}, nil)

	require.NoError(t, paramTracker.Add(fakeParams))
	require.NoError(t, tracker.Create(definitionsGVR, denyPolicy, denyPolicy.Namespace))
	require.NoError(t, tracker.Create(bindingsGVR, denyBinding, denyBinding.Namespace))

	// Wait for controller to reconcile given objects
	require.NoError(t,
		waitForReconcile(
			testContext, controller,
			fakeParams, denyBinding, denyPolicy))

	record := attributeRecord(nil, fakeParams, admission.Create)
	require.ErrorContains(t,
		handler.Validate(
			testContext,
			record,
			&admission.RuntimeObjectInterfaces{},
		),
		`Denied`)

	require.NoError(t, tracker.Delete(definitionsGVR, denyPolicy.Namespace, denyPolicy.Name))
	require.NoError(t, waitForReconcileDeletion(testContext, controller, denyPolicy))

	require.NoError(t, handler.Validate(
		testContext,
		// Object is irrelevant/unchecked for this test. Just test that
		// the evaluator is executed, and returns a denial
		record,
		&admission.RuntimeObjectInterfaces{},
	))
}

// Shows that a binding which is in effect will stop being in effect when removed
func TestRemoveBinding(t *testing.T) {
	testContext, testContextCancel := context.WithCancel(context.Background())
	defer testContextCancel()
	compiler := &fakeCompiler{
		// Match everything by default
		DefaultMatch: true,
	}
	handler, paramTracker, tracker, controller := setupFakeTest(t, compiler)

	datalock := sync.Mutex{}
	numCompiles := 0

	compiler.RegisterDefinition(denyPolicy, func(vap *v1alpha1.ValidatingAdmissionPolicy) Validator {
		datalock.Lock()
		numCompiles += 1
		datalock.Unlock()

		return testValidator{}
	}, nil)

	require.NoError(t, paramTracker.Add(fakeParams))
	require.NoError(t, tracker.Create(definitionsGVR, denyPolicy, denyPolicy.Namespace))
	require.NoError(t, tracker.Create(bindingsGVR, denyBinding, denyBinding.Namespace))

	// Wait for controller to reconcile given objects
	require.NoError(t,
		waitForReconcile(
			testContext, controller,
			fakeParams, denyBinding, denyPolicy))

	record := attributeRecord(nil, fakeParams, admission.Create)

	require.ErrorContains(t,
		handler.Validate(
			testContext,
			record,
			&admission.RuntimeObjectInterfaces{},
		),
		`Denied`)

	//require.Equal(t, []*unstructured.Unstructured{fakeParams}, passedParams)
	require.NoError(t, tracker.Delete(bindingsGVR, denyBinding.Namespace, denyBinding.Name))
	require.NoError(t, waitForReconcileDeletion(testContext, controller, denyBinding))
}

// Shows that an error is surfaced if a paramSource specified in a binding does
// not actually exist
func TestInvalidParamSourceGVK(t *testing.T) {
	testContext, testContextCancel := context.WithCancel(context.Background())
	defer testContextCancel()
	compiler := &fakeCompiler{
		// Match everything by default
		DefaultMatch: true,
	}
	handler, _, tracker, controller := setupFakeTest(t, compiler)
	passedParams := make(chan *unstructured.Unstructured)

	badPolicy := *denyPolicy
	badPolicy.Spec.ParamKind = &v1alpha1.ParamKind{
		APIVersion: paramsGVK.GroupVersion().String(),
		Kind:       "BadParamKind",
	}

	require.NoError(t, tracker.Create(definitionsGVR, &badPolicy, badPolicy.Namespace))
	require.NoError(t, tracker.Create(bindingsGVR, denyBinding, denyBinding.Namespace))

	// Wait for controller to reconcile given objects
	require.NoError(t,
		waitForReconcile(
			testContext, controller,
			denyBinding, &badPolicy))

	err := handler.Validate(
		testContext,
		attributeRecord(nil, fakeParams, admission.Create),
		&admission.RuntimeObjectInterfaces{},
	)

	// expect the specific error to be that the param was not found, not that CRD
	// is not existing
	require.ErrorContains(t, err,
		`failed to configure policy: failed to find resource referenced by paramKind: 'example.com/v1, Kind=BadParamKind'`)

	close(passedParams)
	require.Len(t, passedParams, 0)
}

// Shows that an error is surfaced if a param specified in a binding does not
// actually exist
func TestInvalidParamSourceInstanceName(t *testing.T) {
	testContext, testContextCancel := context.WithCancel(context.Background())
	defer testContextCancel()
	compiler := &fakeCompiler{
		// Match everything by default
		DefaultMatch: true,
	}
	handler, _, tracker, controller := setupFakeTest(t, compiler)

	datalock := sync.Mutex{}
	passedParams := []*unstructured.Unstructured{}
	numCompiles := 0

	compiler.RegisterDefinition(denyPolicy, func(vap *v1alpha1.ValidatingAdmissionPolicy) Validator {
		datalock.Lock()
		numCompiles += 1
		datalock.Unlock()

		return testValidator{}
	}, nil)

	require.NoError(t, tracker.Create(definitionsGVR, denyPolicy, denyPolicy.Namespace))
	require.NoError(t, tracker.Create(bindingsGVR, denyBinding, denyBinding.Namespace))

	// Wait for controller to reconcile given objects
	require.NoError(t,
		waitForReconcile(
			testContext, controller,
			denyBinding, denyPolicy))

	err := handler.Validate(
		testContext,
		attributeRecord(nil, fakeParams, admission.Create),
		&admission.RuntimeObjectInterfaces{},
	)

	// expect the specific error to be that the param was not found, not that CRD
	// is not existing
	require.ErrorContains(t, err,
		`failed to configure binding: replicas-test.example.com not found`)
	require.Len(t, passedParams, 0)
}

// Shows that a definition with no param source works just fine, and has
// nil params passed to its evaluator.
//
// Also shows that if binding has specified params in this instance then they
// are silently ignored.
func TestEmptyParamSource(t *testing.T) {
	testContext, testContextCancel := context.WithCancel(context.Background())
	defer testContextCancel()
	compiler := &fakeCompiler{
		// Match everything by default
		DefaultMatch: true,
	}
	handler, _, tracker, controller := setupFakeTest(t, compiler)

	datalock := sync.Mutex{}
	numCompiles := 0

	// Push some fake
	noParamSourcePolicy := *denyPolicy
	noParamSourcePolicy.Spec.ParamKind = nil

	compiler.RegisterDefinition(&noParamSourcePolicy, func(vap *v1alpha1.ValidatingAdmissionPolicy) Validator {
		datalock.Lock()
		numCompiles += 1
		datalock.Unlock()

		return testValidator{}
	}, nil)

	require.NoError(t, tracker.Create(definitionsGVR, &noParamSourcePolicy, noParamSourcePolicy.Namespace))
	require.NoError(t, tracker.Create(bindingsGVR, denyBindingWithNoParamRef, denyBindingWithNoParamRef.Namespace))

	// Wait for controller to reconcile given objects
	require.NoError(t,
		waitForReconcile(
			testContext, controller,
			denyBinding, denyPolicy))

	err := handler.Validate(
		testContext,
		// Object is irrelevant/unchecked for this test. Just test that
		// the evaluator is executed, and returns a denial
		attributeRecord(nil, fakeParams, admission.Create),
		&admission.RuntimeObjectInterfaces{},
	)

	require.ErrorContains(t, err, `Denied`)
	require.Equal(t, 1, numCompiles)
}

// Shows what happens when multiple policies share one param type, then
// one policy stops using the param. The expectation is the second policy
// keeps behaving normally
func TestMultiplePoliciesSharedParamType(t *testing.T) {
	testContext, testContextCancel := context.WithCancel(context.Background())
	defer testContextCancel()

	compiler := &fakeCompiler{
		// Match everything by default
		DefaultMatch: true,
	}
	handler, paramTracker, tracker, controller := setupFakeTest(t, compiler)

	// Use ConfigMap native-typed param
	policy1 := *denyPolicy
	policy1.Name = "denypolicy1.example.com"

	policy2 := *denyPolicy
	policy2.Name = "denypolicy2.example.com"

	binding1 := *denyBinding
	binding2 := *denyBinding

	binding1.Name = "denybinding1.example.com"
	binding1.Spec.PolicyName = policy1.Name
	binding2.Name = "denybinding2.example.com"
	binding2.Spec.PolicyName = policy2.Name

	compiles1 := atomic.Int64{}
	evaluations1 := atomic.Int64{}

	compiles2 := atomic.Int64{}
	evaluations2 := atomic.Int64{}

	compiler.RegisterDefinition(&policy1, func(vap *v1alpha1.ValidatingAdmissionPolicy) Validator {
		compiles1.Add(1)

		return validatorFunc(func(versionedAttr *whgeneric.VersionedAttributes, versionedParams runtime.Object) ([]PolicyDecision, error) {
			evaluations1.Add(1)
			return []PolicyDecision{
				{
					Action: ActionAdmit,
				},
			}, nil
		})
	}, nil)

	compiler.RegisterDefinition(&policy2, func(vap *v1alpha1.ValidatingAdmissionPolicy) Validator {
		compiles2.Add(1)

		return validatorFunc(func(versionedAttr *whgeneric.VersionedAttributes, versionedParams runtime.Object) ([]PolicyDecision, error) {
			evaluations2.Add(1)
			return []PolicyDecision{
				{
					Action:  ActionDeny,
					Message: "Policy2Denied",
				},
			}, nil
		})
	}, nil)

	require.NoError(t, tracker.Create(definitionsGVR, &policy1, policy1.Namespace))
	require.NoError(t, tracker.Create(bindingsGVR, &binding1, binding1.Namespace))
	require.NoError(t, paramTracker.Add(fakeParams))

	// Wait for controller to reconcile given objects
	require.NoError(t,
		waitForReconcile(
			testContext, controller,
			&binding1, &policy1, fakeParams))

	// Make sure policy 1 is created and bound to the params type first
	require.NoError(t, tracker.Create(definitionsGVR, &policy2, policy2.Namespace))
	require.NoError(t, tracker.Create(bindingsGVR, &binding2, binding2.Namespace))

	// Wait for controller to reconcile given objects
	require.NoError(t,
		waitForReconcile(
			testContext, controller,
			&binding1, &binding2, &policy1, &policy2, fakeParams))

	err := handler.Validate(
		testContext,
		// Object is irrelevant/unchecked for this test. Just test that
		// the evaluator is executed, and returns admit meaning the params
		// passed was a configmap
		attributeRecord(nil, fakeParams, admission.Create),
		&admission.RuntimeObjectInterfaces{},
	)

	require.ErrorContains(t, err, `Denied`)
	require.EqualValues(t, 1, compiles1.Load())
	require.EqualValues(t, 1, evaluations1.Load())
	require.EqualValues(t, 1, compiles2.Load())
	require.EqualValues(t, 1, evaluations2.Load())

	// Remove param type from policy1
	// Show that policy2 evaluator is still being passed the configmaps
	policy1.Spec.ParamKind = nil
	policy1.ResourceVersion = "2"

	binding1.Spec.ParamRef = nil
	binding1.ResourceVersion = "2"

	require.NoError(t, tracker.Update(definitionsGVR, &policy1, policy1.Namespace))
	require.NoError(t, tracker.Update(bindingsGVR, &binding1, binding1.Namespace))
	require.NoError(t,
		waitForReconcile(
			testContext, controller,
			&binding1, &policy1))

	err = handler.Validate(
		testContext,
		// Object is irrelevant/unchecked for this test. Just test that
		// the evaluator is executed, and returns admit meaning the params
		// passed was a configmap
		attributeRecord(nil, fakeParams, admission.Create),
		&admission.RuntimeObjectInterfaces{},
	)

	require.ErrorContains(t, err, `Policy2Denied`)
	require.EqualValues(t, 2, compiles1.Load())
	require.EqualValues(t, 2, evaluations1.Load())
	require.EqualValues(t, 1, compiles2.Load())
	require.EqualValues(t, 2, evaluations2.Load())
}

// Shows that we can refer to native-typed params just fine
// (as opposed to CRD params)
func TestNativeTypeParam(t *testing.T) {
	testContext, testContextCancel := context.WithCancel(context.Background())
	defer testContextCancel()
	compiler := &fakeCompiler{
		// Match everything by default
		DefaultMatch: true,
	}
	handler, _, tracker, controller := setupFakeTest(t, compiler)

	compiles := atomic.Int64{}
	evaluations := atomic.Int64{}

	// Use ConfigMap native-typed param
	nativeTypeParamPolicy := *denyPolicy
	nativeTypeParamPolicy.Spec.ParamKind = &v1alpha1.ParamKind{
		APIVersion: "v1",
		Kind:       "ConfigMap",
	}

	compiler.RegisterDefinition(&nativeTypeParamPolicy, func(vap *v1alpha1.ValidatingAdmissionPolicy) Validator {
		compiles.Add(1)

		return validatorFunc(func(versionedAttr *whgeneric.VersionedAttributes, params runtime.Object) ([]PolicyDecision, error) {
			evaluations.Add(1)

			// show that the passed params was a ConfigMap native type
			if _, ok := params.(*v1.ConfigMap); ok {
				return []PolicyDecision{
					{
						Action:  ActionDeny,
						Message: "correct type",
					},
				}, nil
			}
			return []PolicyDecision{
				{
					Action:  ActionDeny,
					Message: "Incorrect param type",
				},
			}, nil
		})
	}, nil)

	configMapParam := &v1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            "replicas-test.example.com",
			Namespace:       "",
			ResourceVersion: "1",
		},
		Data: map[string]string{
			"coolkey": "coolvalue",
		},
	}
	require.NoError(t, tracker.Create(definitionsGVR, &nativeTypeParamPolicy, nativeTypeParamPolicy.Namespace))
	require.NoError(t, tracker.Create(bindingsGVR, denyBinding, denyBinding.Namespace))
	require.NoError(t, tracker.Add(configMapParam))

	// Wait for controller to reconcile given objects
	require.NoError(t,
		waitForReconcile(
			testContext, controller,
			denyBinding, denyPolicy, configMapParam))

	err := handler.Validate(
		testContext,
		// Object is irrelevant/unchecked for this test. Just test that
		// the evaluator is executed, and returns admit meaning the params
		// passed was a configmap
		attributeRecord(nil, fakeParams, admission.Create),
		&admission.RuntimeObjectInterfaces{},
	)

	require.ErrorContains(t, err, "correct type")
	require.EqualValues(t, 1, compiles.Load())
	require.EqualValues(t, 1, evaluations.Load())
}
