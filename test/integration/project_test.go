package integration

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/printers"
	rbacv1client "k8s.io/client-go/kubernetes/typed/rbac/v1"

	authorizationv1 "github.com/openshift/api/authorization/v1"
	projectv1 "github.com/openshift/api/project/v1"
	authorizationv1client "github.com/openshift/client-go/authorization/clientset/versioned/typed/authorization/v1"
	projectv1client "github.com/openshift/client-go/project/clientset/versioned/typed/project/v1"
	"github.com/openshift/oc/pkg/cli/admin/policy"
	oapi "github.com/openshift/openshift-apiserver/pkg/api"
	testutil "github.com/openshift/origin/test/util"
	testserver "github.com/openshift/origin/test/util/server"
	"k8s.io/kubernetes/openshift-kube-apiserver/authorization/scope"
)

// TestProjectIsNamespace verifies that a project is a namespace, and a namespace is a project
func TestProjectIsNamespace(t *testing.T) {
	masterConfig, clusterAdminKubeConfig, err := testserver.StartTestMasterAPI()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer testserver.CleanupMasterEtcd(t, masterConfig)

	clusterAdminClientConfig, err := testutil.GetClusterAdminClientConfig(clusterAdminKubeConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	clusterAdminProjectClient := projectv1client.NewForConfigOrDie(clusterAdminClientConfig)
	kubeClientset, err := testutil.GetClusterAdminKubeClient(clusterAdminKubeConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// create a namespace
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "integration-test"},
	}
	namespaceResult, err := kubeClientset.CoreV1().Namespaces().Create(namespace)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// now try to get the project with the same name and ensure it is our namespace
	project, err := clusterAdminProjectClient.Projects().Get(namespaceResult.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if project.Name != namespace.Name {
		t.Fatalf("Project name did not match namespace name, project %v, namespace %v", project.Name, namespace.Name)
	}

	// now create a project
	project = &projectv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: "new-project",
			Annotations: map[string]string{
				oapi.OpenShiftDisplayName:    "Hello World",
				"openshift.io/node-selector": "env=test",
			},
		},
	}
	projectResult, err := clusterAdminProjectClient.Projects().Create(project)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// now get the namespace for that project
	namespace, err = kubeClientset.CoreV1().Namespaces().Get(projectResult.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if project.Name != namespace.Name {
		t.Fatalf("Project name did not match namespace name, project %v, namespace %v", project.Name, namespace.Name)
	}
	if project.Annotations[oapi.OpenShiftDisplayName] != namespace.Annotations[oapi.OpenShiftDisplayName] {
		t.Fatalf("Project display name did not match namespace annotation, project %v, namespace %v", project.Annotations[oapi.OpenShiftDisplayName], namespace.Annotations[oapi.OpenShiftDisplayName])
	}
	if project.Annotations["openshift.io/node-selector"] != namespace.Annotations["openshift.io/node-selector"] {
		t.Fatalf("Project node selector did not match namespace node selector, project %v, namespace %v", project.Annotations["openshift.io/node-selector"], namespace.Annotations["openshift.io/node-selector"])
	}
}

func TestProjectWatch(t *testing.T) {
	masterConfig, clusterAdminKubeConfig, err := testserver.StartTestMaster()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer testserver.CleanupMasterEtcd(t, masterConfig)

	clusterAdminClientConfig, err := testutil.GetClusterAdminClientConfig(clusterAdminKubeConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, bobConfig, err := testutil.GetClientForUser(clusterAdminClientConfig, "bob")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bobProjectClient := projectv1client.NewForConfigOrDie(bobConfig)
	w, err := bobProjectClient.Projects().Watch(metav1.ListOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, _, err := testserver.CreateNewProject(clusterAdminClientConfig, "ns-01", "bob"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitForAdd("ns-01", w, t)

	// TEST FOR ADD/REMOVE ACCESS
	_, joeConfig, err := testserver.CreateNewProject(clusterAdminClientConfig, "ns-02", "joe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	addBob := &policy.RoleModificationOptions{
		RoleBindingNamespace: "ns-02",
		RoleName:             "edit",
		RoleKind:             "ClusterRole",
		RbacClient:           rbacv1client.NewForConfigOrDie(joeConfig),
		Users:                []string{"bob"},
		PrintFlags:           genericclioptions.NewPrintFlags(""),
		ToPrinter:            func(string) (printers.ResourcePrinter, error) { return printers.NewDiscardingPrinter(), nil },
	}
	if err := addBob.AddRole(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitForAdd("ns-02", w, t)

	if err := addBob.RemoveRole(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitForDelete("ns-02", w, t)

	// TEST FOR DELETE PROJECT
	if _, _, err := testserver.CreateNewProject(clusterAdminClientConfig, "ns-03", "bob"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitForAdd("ns-03", w, t)

	if err := bobProjectClient.Projects().Delete("ns-03", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// wait for the delete
	waitForDelete("ns-03", w, t)

	// test the "start from beginning watch"
	beginningWatch, err := bobProjectClient.Projects().Watch(metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitForAdd("ns-01", beginningWatch, t)

	fromNowWatch, err := bobProjectClient.Projects().Watch(metav1.ListOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case event := <-fromNowWatch.ResultChan():
		t.Fatalf("unexpected event %s %#v", event.Type, event.Object)

	case <-time.After(3 * time.Second):
	}
}

func TestProjectWatchWithSelectionPredicate(t *testing.T) {
	masterConfig, clusterAdminKubeConfig, err := testserver.StartTestMaster()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer testserver.CleanupMasterEtcd(t, masterConfig)

	clusterAdminClientConfig, err := testutil.GetClusterAdminClientConfig(clusterAdminKubeConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, bobConfig, err := testutil.GetClientForUser(clusterAdminClientConfig, "bob")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bobProjectClient := projectv1client.NewForConfigOrDie(bobConfig)
	w, err := bobProjectClient.Projects().Watch(metav1.ListOptions{
		FieldSelector: "metadata.name=ns-01",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, _, err := testserver.CreateNewProject(clusterAdminClientConfig, "ns-01", "bob"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// we should be seeing an "ADD" watch event being emitted, since we are specifically watching this project via a field selector
	waitForAdd("ns-01", w, t)

	if _, _, err := testserver.CreateNewProject(clusterAdminClientConfig, "ns-03", "bob"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// we are only watching ns-01, we should not receive events for other projects
	waitForNoEvent(w, "ns-01", t)

	if err := bobProjectClient.Projects().Delete("ns-03", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// we are only watching ns-01, we should not receive events for other projects
	waitForNoEvent(w, "ns-01", t)

	// test the "start from beginning watch"
	beginningWatch, err := bobProjectClient.Projects().Watch(metav1.ListOptions{
		ResourceVersion: "0",
		FieldSelector:   "metadata.name=ns-01",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// we should be seeing an "ADD" watch event being emitted, since we are specifically watching this project via a field selector
	waitForAdd("ns-01", beginningWatch, t)

	fromNowWatch, err := bobProjectClient.Projects().Watch(metav1.ListOptions{
		FieldSelector: "metadata.name=ns-01",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// since we are only watching for events from "ns-01", and no projects are being modified, we should not receive any events here
	waitForNoEvent(fromNowWatch, "ns-01", t)
}

// waitForNoEvent ensures no stray events come in.  skipProject allows modify events only for the named project
func waitForNoEvent(w watch.Interface, skipProject string, t *testing.T) {
	for {
		select {
		case event := <-w.ResultChan():
			if event.Type != watch.Modified {
				t.Fatalf("unexpected event %v with object %#v", event, event.Object)
			}
			project, ok := event.Object.(*projectv1.Project)
			if !ok {
				t.Fatalf("unexpected event %v with object %#v", event, event.Object)
			}
			t.Logf("got %#v %#v", event, project)
			if project.Name != skipProject {
				t.Fatalf("unexpected event %v with object %#v", event, event.Object)
			}

			continue
		case <-time.After(3 * time.Second):
			return
		}
	}
}

func waitForDelete(projectName string, w watch.Interface, t *testing.T) {
	for {
		select {
		case event := <-w.ResultChan():
			project := event.Object.(*projectv1.Project)
			t.Logf("got %#v %#v", event, project)
			if event.Type == watch.Deleted && project.Name == projectName {
				return
			}

		case <-time.After(30 * time.Second):
			t.Fatalf("timeout: %v", projectName)
		}
	}
}
func waitForAdd(projectName string, w watch.Interface, t *testing.T) {
	for {
		select {
		case event := <-w.ResultChan():
			project := event.Object.(*projectv1.Project)
			t.Logf("got %#v %#v", event, project)
			if event.Type == watch.Added && project.Name == projectName {
				return
			}

		case <-time.After(30 * time.Second):
			t.Fatalf("timeout: %v", projectName)
		}
	}
}

func waitForOnlyAdd(projectName string, w watch.Interface, t *testing.T) {
	for {
		select {
		case event := <-w.ResultChan():
			project := event.Object.(*projectv1.Project)
			t.Logf("got %#v %#v", event, project)
			if project.Name == projectName {
				// the first event we see for the expected project must be an ADD
				if event.Type == watch.Added {
					return
				}
				t.Fatalf("got unexpected project ADD waiting for %s: %v", project.Name, event)
			}
			if event.Type == watch.Modified {
				// ignore modifications from other projects
				continue
			}
			t.Fatalf("got unexpected project %v", project.Name)

		case <-time.After(30 * time.Second):
			t.Fatalf("timeout: %v", projectName)
		}
	}
}
func waitForOnlyDelete(projectName string, w watch.Interface, t *testing.T) {
	hasTerminated := sets.NewString()
	for {
		select {
		case event := <-w.ResultChan():
			project := event.Object.(*projectv1.Project)
			t.Logf("got %#v %#v", event, project)
			if project.Name == projectName {
				if event.Type == watch.Deleted {
					return
				}
				// if its an event indicating Terminated status, don't fail, but keep waiting
				if event.Type == watch.Modified {
					terminating := project.Status.Phase == corev1.NamespaceTerminating
					if !terminating && hasTerminated.Has(project.Name) {
						t.Fatalf("project %s was terminating, but then got an event where it was not terminating: %#v", project.Name, project)
					}
					if terminating {
						hasTerminated.Insert(project.Name)
					}
					continue
				}
			}
			if event.Type == watch.Modified {
				// ignore modifications for other projects
				continue
			}
			t.Fatalf("got unexpected project %v", project.Name)

		case <-time.After(30 * time.Second):
			t.Fatalf("timeout: %v", projectName)
		}
	}
}

func TestScopedProjectAccess(t *testing.T) {
	masterConfig, clusterAdminKubeConfig, err := testserver.StartTestMaster()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer testserver.CleanupMasterEtcd(t, masterConfig)

	clusterAdminClientConfig, err := testutil.GetClusterAdminClientConfig(clusterAdminKubeConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, fullBobConfig, err := testutil.GetClientForUser(clusterAdminClientConfig, "bob")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fullBobClient := projectv1client.NewForConfigOrDie(fullBobConfig)

	_, oneTwoBobConfig, err := testutil.GetScopedClientForUser(clusterAdminClientConfig, "bob", []string{
		scope.UserListScopedProjects,
		scope.ClusterRoleIndicator + "view:one",
		scope.ClusterRoleIndicator + "view:two",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	oneTwoBobClient := projectv1client.NewForConfigOrDie(oneTwoBobConfig)

	_, twoThreeBobConfig, err := testutil.GetScopedClientForUser(clusterAdminClientConfig, "bob", []string{
		scope.UserListScopedProjects,
		scope.ClusterRoleIndicator + "view:two",
		scope.ClusterRoleIndicator + "view:three",
	})
	twoThreeBobClient := projectv1client.NewForConfigOrDie(twoThreeBobConfig)

	_, allBobConfig, err := testutil.GetScopedClientForUser(clusterAdminClientConfig, "bob", []string{
		scope.UserListScopedProjects,
		scope.ClusterRoleIndicator + "view:*",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	allBobClient := projectv1client.NewForConfigOrDie(allBobConfig)

	oneTwoWatch, err := oneTwoBobClient.Projects().Watch(metav1.ListOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	twoThreeWatch, err := twoThreeBobClient.Projects().Watch(metav1.ListOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	allWatch, err := allBobClient.Projects().Watch(metav1.ListOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, _, err := testserver.CreateNewProject(clusterAdminClientConfig, "one", "bob"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Logf("test 1")
	waitForOnlyAdd("one", allWatch, t)
	waitForOnlyAdd("one", oneTwoWatch, t)

	if _, _, err := testserver.CreateNewProject(clusterAdminClientConfig, "two", "bob"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Logf("test 2")
	waitForOnlyAdd("two", allWatch, t)
	waitForOnlyAdd("two", oneTwoWatch, t)
	waitForOnlyAdd("two", twoThreeWatch, t)

	if _, _, err := testserver.CreateNewProject(clusterAdminClientConfig, "three", "bob"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Logf("test 3")
	waitForOnlyAdd("three", allWatch, t)
	waitForOnlyAdd("three", twoThreeWatch, t)

	if _, _, err := testserver.CreateNewProject(clusterAdminClientConfig, "four", "bob"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitForOnlyAdd("four", allWatch, t)

	if err := hasExactlyTheseProjects(oneTwoBobClient.Projects(), sets.NewString("one", "two")); err != nil {
		t.Error(err)
	}

	if err := hasExactlyTheseProjects(twoThreeBobClient.Projects(), sets.NewString("two", "three")); err != nil {
		t.Error(err)
	}

	if err := hasExactlyTheseProjects(allBobClient.Projects(), sets.NewString("one", "two", "three", "four")); err != nil {
		t.Error(err)
	}

	if err := fullBobClient.Projects().Delete("four", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitForOnlyDelete("four", allWatch, t)

	if err := fullBobClient.Projects().Delete("three", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitForOnlyDelete("three", allWatch, t)
	waitForOnlyDelete("three", twoThreeWatch, t)

	if err := fullBobClient.Projects().Delete("two", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitForOnlyDelete("two", allWatch, t)
	waitForOnlyDelete("two", oneTwoWatch, t)
	waitForOnlyDelete("two", twoThreeWatch, t)

	if err := fullBobClient.Projects().Delete("one", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	waitForOnlyDelete("one", allWatch, t)
	waitForOnlyDelete("one", oneTwoWatch, t)
}

func TestInvalidRoleRefs(t *testing.T) {
	masterConfig, clusterAdminKubeConfig, err := testserver.StartTestMaster()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer testserver.CleanupMasterEtcd(t, masterConfig)

	clusterAdminClientConfig, err := testutil.GetClusterAdminClientConfig(clusterAdminKubeConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	clusterAdminRbacClient := rbacv1client.NewForConfigOrDie(clusterAdminClientConfig)
	clusterAdminAuthorizationClient := authorizationv1client.NewForConfigOrDie(clusterAdminClientConfig)

	_, bobConfig, err := testutil.GetClientForUser(clusterAdminClientConfig, "bob")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, aliceConfig, err := testutil.GetClientForUser(clusterAdminClientConfig, "alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, _, err := testserver.CreateNewProject(clusterAdminClientConfig, "foo", "bob"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, _, err := testserver.CreateNewProject(clusterAdminClientConfig, "bar", "alice"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	roleName := "missing-role"
	if _, err := clusterAdminRbacClient.ClusterRoles().Create(&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: roleName}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	modifyRole := &policy.RoleModificationOptions{
		RoleName:   roleName,
		RoleKind:   "ClusterRole",
		RbacClient: clusterAdminRbacClient,
		Users:      []string{"someuser"},
		PrintFlags: genericclioptions.NewPrintFlags(""),
		ToPrinter:  func(string) (printers.ResourcePrinter, error) { return printers.NewDiscardingPrinter(), nil },
	}
	// mess up rolebindings in "foo"
	modifyRole.RoleBindingNamespace = "foo"
	if err := modifyRole.AddRole(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// mess up rolebindings in "bar"
	modifyRole.RoleBindingNamespace = "bar"
	if err := modifyRole.AddRole(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// mess up clusterrolebindings
	modifyRole.RoleBindingNamespace = ""
	if err := modifyRole.AddRole(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Orphan the rolebindings by deleting the role
	if err := clusterAdminRbacClient.ClusterRoles().Delete(roleName, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// wait for evaluation errors to show up in both namespaces and at cluster scope
	if err := wait.PollImmediate(100*time.Millisecond, 10*time.Second, func() (bool, error) {
		review := &authorizationv1.ResourceAccessReview{Action: authorizationv1.Action{Verb: "get", Resource: "pods"}}
		review.Action.Namespace = "foo"
		if resp, err := clusterAdminAuthorizationClient.ResourceAccessReviews().Create(review); err != nil || resp.EvaluationError == "" {
			return false, err
		}
		review.Action.Namespace = "bar"
		if resp, err := clusterAdminAuthorizationClient.ResourceAccessReviews().Create(review); err != nil || resp.EvaluationError == "" {
			return false, err
		}
		review.Action.Namespace = ""
		if resp, err := clusterAdminAuthorizationClient.ResourceAccessReviews().Create(review); err != nil || resp.EvaluationError == "" {
			return false, err
		}
		return true, nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Make sure bob still sees his project (and only his project)
	if hasErr := hasExactlyTheseProjects(projectv1client.NewForConfigOrDie(bobConfig).Projects(), sets.NewString("foo")); hasErr != nil {
		t.Error(hasErr)
	}
	// Make sure alice still sees her project (and only her project)
	if hasErr := hasExactlyTheseProjects(projectv1client.NewForConfigOrDie(aliceConfig).Projects(), sets.NewString("bar")); hasErr != nil {
		t.Error(hasErr)
	}
	// Make sure cluster admin still sees all projects
	if projects, err := projectv1client.NewForConfigOrDie(clusterAdminClientConfig).Projects().List(metav1.ListOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	} else {
		projectNames := sets.NewString()
		for _, project := range projects.Items {
			projectNames.Insert(project.Name)
		}
		if !projectNames.HasAll("foo", "bar", "openshift-infra", "openshift", "default") {
			t.Errorf("Expected projects foo and bar, got %v", projectNames.List())
		}
	}
}

func hasExactlyTheseProjects(lister projectv1client.ProjectInterface, projects sets.String) error {
	var lastErr error
	if err := wait.PollImmediate(100*time.Millisecond, 10*time.Second, func() (bool, error) {
		list, err := lister.List(metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		if len(list.Items) != len(projects) {
			lastErr = fmt.Errorf("expected %v, got %v", projects.List(), list.Items)
			return false, nil
		}
		for _, project := range list.Items {
			if !projects.Has(project.Name) {
				lastErr = fmt.Errorf("expected %v, got %v", projects.List(), list.Items)
				return false, nil
			}
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("hasExactlyTheseProjects failed with %v and %v", err, lastErr)
	}
	return nil
}
