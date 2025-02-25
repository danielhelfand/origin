package integration

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	appsv1 "github.com/openshift/api/apps/v1"
	"github.com/openshift/api/image/docker10"
	imagev1 "github.com/openshift/api/image/v1"
	imageclientv1 "github.com/openshift/client-go/image/clientset/versioned"
	imagev1client "github.com/openshift/client-go/image/clientset/versioned"
	"github.com/openshift/library-go/pkg/image/imageutil"
	stratsupport "github.com/openshift/oc/pkg/cli/deployer/strategy/support"
	imagetest "github.com/openshift/openshift-apiserver/pkg/image/apiserver/testutil"
	testutil "github.com/openshift/origin/test/util"
	testserver "github.com/openshift/origin/test/util/server"
)

func mockImageStream() *imagev1.ImageStream {
	return &imagev1.ImageStream{ObjectMeta: metav1.ObjectMeta{Name: "test"}}
}

func TestImageStreamMappingCreate(t *testing.T) {
	masterConfig, clusterAdminKubeConfig, err := testserver.StartTestMasterAPI()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer testserver.CleanupMasterEtcd(t, masterConfig)

	clusterAdminClientConfig, err := testutil.GetClusterAdminClientConfig(clusterAdminKubeConfig)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	clusterAdminImageClient := imagev1client.NewForConfigOrDie(clusterAdminClientConfig).ImageV1()
	err = testutil.CreateNamespace(clusterAdminKubeConfig, testutil.Namespace())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	stream := mockImageStream()

	expected, err := clusterAdminImageClient.ImageStreams(testutil.Namespace()).Create(stream)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if expected.Name == "" {
		t.Errorf("Unexpected empty image Name %v", expected)
	}

	// create a mapping to an image that doesn't exist
	mapping := &imagev1.ImageStreamMapping{
		ObjectMeta: metav1.ObjectMeta{Name: stream.Name},
		Tag:        "newer",
		Image: imagev1.Image{
			ObjectMeta: metav1.ObjectMeta{
				Name: "image1",
			},
			DockerImageReference: "some/other/name",
		},
	}
	if _, err := clusterAdminImageClient.ImageStreamMappings(testutil.Namespace()).Create(mapping); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// verify we can tag a second time with the same data, and nothing changes
	if _, err := clusterAdminImageClient.ImageStreamMappings(testutil.Namespace()).Create(mapping); err != nil {
		t.Fatalf("unexpected non-error or type: %v", err)
	}

	// create an image directly
	image := &imagev1.Image{
		ObjectMeta: metav1.ObjectMeta{Name: "image2"},
		DockerImageMetadata: runtime.RawExtension{
			Object: &docker10.DockerImage{
				Config: &docker10.DockerConfig{
					Env: []string{"A=B"},
				},
			},
		},
	}
	if _, err := clusterAdminImageClient.Images().Create(image); err == nil {
		t.Error("unexpected non-error")
	}
	image.DockerImageReference = "some/other/name" // can reuse references across multiple images
	actual, err := clusterAdminImageClient.Images().Create(image)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if actual == nil || actual.Name != image.Name {
		t.Errorf("unexpected object: %#v", actual)
	}

	// verify that image stream mappings cannot mutate / overwrite the image (images are immutable)
	mapping = &imagev1.ImageStreamMapping{
		ObjectMeta: metav1.ObjectMeta{Name: stream.Name},
		Tag:        "newest",
		Image:      *image,
	}
	mapping.Image.DockerImageReference = "different"
	if _, err := clusterAdminImageClient.ImageStreamMappings(testutil.Namespace()).Create(mapping); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	image, err = clusterAdminImageClient.Images().Get(image.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if image.DockerImageReference != "some/other/name" {
		t.Fatalf("image was unexpectedly mutated: %#v", image)
	}

	// ensure the correct tags are set
	updated, err := clusterAdminImageClient.ImageStreams(testutil.Namespace()).Get(stream.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if updated.Spec.Tags != nil && len(updated.Spec.Tags) > 0 {
		t.Errorf("unexpected object: %#v", updated.Spec.Tags)
	}

	fromTag, err := clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Get(stream.Name+":newer", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if fromTag.Name != "test:newer" || fromTag.Image.UID == "" || fromTag.Image.DockerImageReference != "some/other/name" {
		t.Errorf("unexpected object: %#v", fromTag)
	}

	fromTag, err = clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Get(stream.Name+":newest", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if fromTag.Name != "test:newest" || fromTag.Image.UID == "" || fromTag.Image.DockerImageReference != "different" {
		t.Errorf("unexpected object: %#v", fromTag)
	}

	// verify that image stream mappings can use the same image for different tags
	image.ResourceVersion = ""
	mapping = &imagev1.ImageStreamMapping{
		ObjectMeta: metav1.ObjectMeta{Name: stream.Name},
		Tag:        "anothertag",
		Image:      *image,
	}
	if _, err := clusterAdminImageClient.ImageStreamMappings(testutil.Namespace()).Create(mapping); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// ensure the correct tags are set
	updated, err = clusterAdminImageClient.ImageStreams(testutil.Namespace()).Get(stream.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if updated.Spec.Tags != nil && len(updated.Spec.Tags) > 0 {
		t.Errorf("unexpected object: %#v", updated.Spec.Tags)
	}

	if _, err := clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Get(stream.Name+":doesnotexist", metav1.GetOptions{}); err == nil || !errors.IsNotFound(err) {
		t.Fatalf("Unexpected error: %v", err)
	}

	fromTag, err = clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Get(stream.Name+":newer", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if fromTag.Name != "test:newer" || fromTag.Image.UID == "" || fromTag.Image.DockerImageReference != "some/other/name" {
		t.Errorf("unexpected object: %#v", fromTag)
	}

	fromTag, err = clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Get(stream.Name+":newest", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if fromTag.Name != "test:newest" || fromTag.Image.UID == "" || fromTag.Image.DockerImageReference != "different" {
		t.Errorf("unexpected object: %#v", fromTag)
	}
	fromTag, err = clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Get(stream.Name+":anothertag", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if fromTag.Name != "test:anothertag" || fromTag.Image.UID == "" || fromTag.Image.DockerImageReference != "some/other/name" {
		t.Errorf("unexpected object: %#v", fromTag)
	}

	// try an update with an incorrect resource version
	if _, err := clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Update(&imagev1.ImageStreamTag{
		ObjectMeta: metav1.ObjectMeta{Namespace: stream.Namespace, Name: stream.Name + ":brandnew", ResourceVersion: fromTag.ResourceVersion + "0"},
		Tag: &imagev1.TagReference{
			From: &corev1.ObjectReference{
				Kind: "ImageStreamTag",
				Name: "newest",
			},
		},
	}); !errors.IsConflict(err) {
		t.Fatalf("should have returned conflict error: %v", err)
	}

	// update and create a new tag
	fromTag, err = clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Update(&imagev1.ImageStreamTag{
		ObjectMeta: metav1.ObjectMeta{Namespace: stream.Namespace, Name: stream.Name + ":brandnew", ResourceVersion: fromTag.ResourceVersion},
		Tag: &imagev1.TagReference{
			From: &corev1.ObjectReference{
				Kind: "ImageStreamTag",
				Name: "newest",
			},
		},
	})
	if err != nil {
		t.Fatalf("should have returned conflict error: %v", err)
	}
	if fromTag.Name != "test:brandnew" || fromTag.Image.UID == "" || fromTag.Tag.From.Name != "newest" {
		t.Errorf("unexpected object: %#v", fromTag)
	}
}

func TestImageStreamWithoutDockerImageConfig(t *testing.T) {
	masterConfig, clusterAdminKubeConfig, err := testserver.StartTestMasterAPI()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer testserver.CleanupMasterEtcd(t, masterConfig)

	clusterAdminClientConfig, err := testutil.GetClusterAdminClientConfig(clusterAdminKubeConfig)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	clusterAdminImageClient := imagev1client.NewForConfigOrDie(clusterAdminClientConfig).ImageV1()
	err = testutil.CreateNamespace(clusterAdminKubeConfig, testutil.Namespace())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	stream := mockImageStream()

	expected, err := clusterAdminImageClient.ImageStreams(testutil.Namespace()).Create(stream)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if expected.Name == "" {
		t.Errorf("Unexpected empty image Name %v", expected)
	}

	imageConfig := docker10.DockerConfig{
		Hostname: "example.com",
		Env:      []string{"A=B"},
	}

	imageConfigBytes, err := json.Marshal(imageConfig)
	if err != nil {
		t.Fatalf("error marshaling image config: %s", err)
	}

	image := imagev1.Image{
		ObjectMeta: metav1.ObjectMeta{
			Name: imagetest.BaseImageWith1LayerDigest,
		},
		DockerImageMetadata: runtime.RawExtension{
			Object: &docker10.DockerImage{
				Config: &docker10.DockerConfig{
					Hostname: "example.com",
					Env:      []string{"A=B"},
				},
			},
		},
		DockerImageConfig:    string(imageConfigBytes),
		DockerImageReference: "some/namespace/name",
	}

	// create a mapping to an image that doesn't exist
	mapping := &imagev1.ImageStreamMapping{
		ObjectMeta: metav1.ObjectMeta{
			Name: stream.Name,
		},
		Tag:   "newer",
		Image: image,
	}
	if _, err := clusterAdminImageClient.ImageStreamMappings(testutil.Namespace()).Create(mapping); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	img, err := clusterAdminImageClient.Images().Get(image.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img.Name != image.Name {
		t.Fatalf("unexpected image: %#v", img)
	}
	if len(img.DockerImageConfig) == 0 {
		t.Fatalf("image has an empty config: %#v", img)
	}

	ist, err := clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Get(stream.Name+":newer", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if ist.Image.Name != image.Name {
		t.Fatalf("unexpected image: %#v", img)
	}
	if len(ist.Image.DockerImageConfig) != 0 {
		t.Errorf("image has a not empty config: %#v", ist)
	}

	isi, err := clusterAdminImageClient.ImageStreamImages(testutil.Namespace()).Get(imageutil.JoinImageStreamImage(stream.Name, imagetest.BaseImageWith1LayerDigest), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if isi.Image.Name != image.Name {
		t.Fatalf("unexpected image: %#v", img)
	}
	if len(isi.Image.DockerImageConfig) != 0 {
		t.Errorf("image has a not empty config: %#v", isi)
	}

}

func TestImageStreamTagLifecycleHook(t *testing.T) {
	masterConfig, clusterAdminKubeConfig, err := testserver.StartTestMaster()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer testserver.CleanupMasterEtcd(t, masterConfig)

	clusterAdminClientConfig, err := testutil.GetClusterAdminClientConfig(clusterAdminKubeConfig)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	clusterAdminImageClient := imagev1client.NewForConfigOrDie(clusterAdminClientConfig).ImageV1()

	err = testutil.CreateNamespace(clusterAdminKubeConfig, testutil.Namespace())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	stream := mockImageStream()
	if _, err := clusterAdminImageClient.ImageStreams(testutil.Namespace()).Create(stream); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	imageClientset := imageclientv1.NewForConfigOrDie(testutil.GetClusterAdminClientConfigOrDie(clusterAdminKubeConfig))
	coreClient := kubernetes.NewForConfigOrDie(testutil.GetClusterAdminClientConfigOrDie(clusterAdminKubeConfig))

	// can tag to a stream that exists
	exec := stratsupport.NewHookExecutor(coreClient, imageClientset.ImageV1(), os.Stdout)
	err = exec.Execute(
		&appsv1.LifecycleHook{
			TagImages: []appsv1.TagImageHook{
				{
					ContainerName: "test",
					To:            corev1.ObjectReference{Kind: "ImageStreamTag", Name: stream.Name + ":test"},
				},
			},
		},
		&corev1.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{Name: "rc-1", Namespace: testutil.Namespace()},
			Spec: corev1.ReplicationControllerSpec{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test",
								Image: "someimage:other",
							},
						},
					},
				},
			},
		},
		"test", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if stream, err = clusterAdminImageClient.ImageStreams(testutil.Namespace()).Get(stream.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if tag, ok := imageutil.SpecHasTag(stream, "test"); !ok || tag.From == nil || tag.From.Name != "someimage:other" {
		t.Fatalf("unexpected object: %#v", tag)
	}

	// can execute a second time the same tag and it should work
	exec = stratsupport.NewHookExecutor(coreClient, imageClientset.ImageV1(), os.Stdout)
	err = exec.Execute(
		&appsv1.LifecycleHook{
			TagImages: []appsv1.TagImageHook{
				{
					ContainerName: "test",
					To:            corev1.ObjectReference{Kind: "ImageStreamTag", Name: stream.Name + ":test"},
				},
			},
		},
		&corev1.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{Name: "rc-1", Namespace: testutil.Namespace()},
			Spec: corev1.ReplicationControllerSpec{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test",
								Image: "someimage:other",
							},
						},
					},
				},
			},
		},
		"test", "test",
	)
	if err != nil {
		t.Fatal(err)
	}

	// can lifecycle tag a new image stream
	exec = stratsupport.NewHookExecutor(coreClient, imageClientset.ImageV1(), os.Stdout)
	err = exec.Execute(
		&appsv1.LifecycleHook{
			TagImages: []appsv1.TagImageHook{
				{
					ContainerName: "test",
					To:            corev1.ObjectReference{Kind: "ImageStreamTag", Name: "test2:test"},
				},
			},
		},
		&corev1.ReplicationController{
			ObjectMeta: metav1.ObjectMeta{Name: "rc-1", Namespace: testutil.Namespace()},
			Spec: corev1.ReplicationControllerSpec{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "test",
								Image: "someimage:other",
							},
						},
					},
				},
			},
		},
		"test", "test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if stream, err = clusterAdminImageClient.ImageStreams(testutil.Namespace()).Get("test2", metav1.GetOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tag, ok := imageutil.SpecHasTag(stream, "test"); !ok || tag.From == nil || tag.From.Name != "someimage:other" {
		t.Fatalf("unexpected object: %#v", tag)
	}
}

func TestRegistryWhitelistingValidation(t *testing.T) {
	testutil.AddAdditionalAllowedRegistries("my.insecure.registry:80")
	masterConfig, clusterAdminKubeConfig, err := testserver.StartTestMasterAPI()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer testserver.CleanupMasterEtcd(t, masterConfig)

	clusterAdminClientConfig, err := testutil.GetClusterAdminClientConfig(clusterAdminKubeConfig)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	clusterAdminImageClient := imagev1client.NewForConfigOrDie(clusterAdminClientConfig).ImageV1()
	err = testutil.CreateNamespace(clusterAdminKubeConfig, testutil.Namespace())
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	stream := mockImageStream()
	stream.Spec = imagev1.ImageStreamSpec{
		Tags: []imagev1.TagReference{
			{
				Name: "latest",
				From: &corev1.ObjectReference{
					Kind: "DockerImage",
					Name: "my.test.registry/repo/sitory:latest",
				},
			},
		},
	}

	_, err = clusterAdminImageClient.ImageStreams(testutil.Namespace()).Create(stream)
	if err == nil || !errors.IsInvalid(err) {
		t.Fatalf("expected invalid error, got: %T %v", err, err)
	}
	if e, a := `spec.tags[latest].from.name: Forbidden: registry "my.test.registry" not allowed by whitelist`, err.Error(); !strings.Contains(a, e) {
		t.Fatalf("expected string %q not contained in error: %s", e, a)
	}

	latestSpecTag, _ := imageutil.SpecHasTag(stream, "latest")
	latestSpecTag.From.Name = "docker.io/busybox"
	upsertSpecTag(&stream.Spec.Tags, latestSpecTag)
	stream, err = clusterAdminImageClient.ImageStreams(testutil.Namespace()).Create(stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	upsertSpecTag(&stream.Spec.Tags, imagev1.TagReference{
		Name: "fail",
		From: &corev1.ObjectReference{
			Kind: "DockerImage",
			Name: "this.will.fail/repo:tag",
		},
	})
	_, err = clusterAdminImageClient.ImageStreams(testutil.Namespace()).Update(stream)
	if err == nil || !errors.IsInvalid(err) {
		t.Fatalf("expected invalid error, got: %T %v", err, err)
	}
	if e, a := `spec.tags[fail].from.name: Forbidden: registry "this.will.fail" not allowed by whitelist`, err.Error(); !strings.Contains(a, e) {
		t.Fatalf("expected string %q not contained in error: %s", e, a)
	}

	stream.Annotations = map[string]string{imagev1.InsecureRepositoryAnnotation: "true"}
	newTags := []imagev1.TagReference{}
	for i := range stream.Spec.Tags {
		if stream.Spec.Tags[i].Name != "fail" {
			newTags = append(newTags, stream.Spec.Tags[i])
		}
	}
	stream.Spec.Tags = newTags

	upsertSpecTag(&stream.Spec.Tags, imagev1.TagReference{
		Name: "pass",
		From: &corev1.ObjectReference{
			Kind: "DockerImage",
			Name: "127.0.0.1:5000/repo:tag",
		},
	})
	_, err = clusterAdminImageClient.ImageStreams(testutil.Namespace()).Update(stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	istag := &imagev1.ImageStreamTag{
		ObjectMeta: metav1.ObjectMeta{
			Name: stream.Name + ":new",
		},
		Tag: &imagev1.TagReference{
			Name: "new",
			From: &corev1.ObjectReference{
				Kind: "DockerImage",
				Name: "my.insecure.registry/repo:new",
			},
		},
	}

	_, err = clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Create(istag)
	if err == nil || !errors.IsInvalid(err) {
		t.Fatalf("expected invalid error, got: %T %v", err, err)
	}
	if e, a := `tag.from.name: Forbidden: registry "my.insecure.registry" not allowed by whitelist`, err.Error(); !strings.Contains(a, e) {
		t.Fatalf("expected string %q not contained in error: %s", e, a)
	}

	istag.Annotations = map[string]string{imagev1.InsecureRepositoryAnnotation: "true"}
	istag, err = clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Create(istag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	istag.Tag.From = &corev1.ObjectReference{
		Kind: "DockerImage",
		Name: "example.com/repo:tag",
	}
	istag.ObjectMeta = metav1.ObjectMeta{
		Name:            istag.Name,
		ResourceVersion: istag.ResourceVersion,
	}
	_, err = clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Update(istag)
	if err == nil || !errors.IsInvalid(err) {
		t.Fatalf("expected invalid error, got: %T %v", err, err)
	}
	if e, a := `tag.from.name: Forbidden: registry "example.com" not allowed by whitelist`, err.Error(); !strings.Contains(a, e) {
		t.Fatalf("expected string %q not contained in error: %s", e, a)
	}

	istag.Tag.From = &corev1.ObjectReference{
		Kind: "DockerImage",
		Name: "myupstream/repo:latest",
	}
	_, err = clusterAdminImageClient.ImageStreamTags(testutil.Namespace()).Update(istag)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
