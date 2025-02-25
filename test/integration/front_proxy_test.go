package integration

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/authentication/user"

	authorizationv1 "github.com/openshift/api/authorization/v1"
	projectv1 "github.com/openshift/api/project/v1"
	authorizationv1client "github.com/openshift/client-go/authorization/clientset/versioned"
	projectv1client "github.com/openshift/client-go/project/clientset/versioned/typed/project/v1"
	"github.com/openshift/library-go/pkg/crypto"
	authorizationapi "github.com/openshift/openshift-apiserver/pkg/authorization/apis/authorization"
	testutil "github.com/openshift/origin/test/util"
	testserver "github.com/openshift/origin/test/util/server"
	"github.com/openshift/origin/test/util/server/deprecated_openshift/apis/config"
	"github.com/openshift/origin/test/util/server/deprecated_openshift/deprecatedcerts"
)

func TestFrontProxy(t *testing.T) {
	t.Skip("skipping until auth team figures this out in the new split API setup, see https://bugzilla.redhat.com/show_bug.cgi?id=1640351")
	masterConfig, err := testserver.DefaultMasterOptions()
	if err != nil {
		t.Fatal(err)
	}
	defer testserver.CleanupMasterEtcd(t, masterConfig)

	frontProxyCAPrefix := "frontproxycatest"
	proxyCertCommonName := "frontproxycerttest"
	proxyUserHeader := "X-Remote-User"
	proxyGroupHeader := "X-Remote-Group"

	certDir, err := ioutil.TempDir("", "frontproxycatestdir")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(certDir)
	t.Logf("cert dir is %s\n", certDir)

	frontProxyClientCA, err := createCA(certDir, frontProxyCAPrefix)
	if err != nil {
		t.Fatal(err)
	}

	proxyCert, err := createClientCert(proxyCertCommonName, certDir, frontProxyCAPrefix)
	if err != nil {
		t.Fatal(err)
	}

	masterConfig.AuthConfig.RequestHeader = &config.RequestHeaderAuthenticationOptions{
		ClientCA:          frontProxyClientCA,
		ClientCommonNames: []string{proxyCertCommonName},

		// These don't get defaulted because we don't round trip config // TODO fix?
		UsernameHeaders: []string{proxyUserHeader},
		GroupHeaders:    []string{proxyGroupHeader},
	}

	clusterAdminKubeConfig, err := testserver.StartConfiguredMasterAPI(masterConfig)
	if err != nil {
		t.Fatal(err)
	}
	clusterAdminClientConfig, err := testutil.GetClusterAdminClientConfig(clusterAdminKubeConfig)
	if err != nil {
		t.Fatal(err)
	}
	clusterAdminAuthorizationClient := authorizationv1client.NewForConfigOrDie(clusterAdminClientConfig).AuthorizationV1()

	proxyHTTPHandler, err := newFrontProxyHandler(clusterAdminClientConfig.Host, masterConfig.ServingInfo.ClientCA, proxyUserHeader, proxyGroupHeader, proxyCert)
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(proxyHTTPHandler)
	defer proxyServer.Close()
	t.Logf("front proxy server is on %v\n", proxyServer.URL)

	w, err := projectv1client.NewForConfigOrDie(clusterAdminClientConfig).Projects().Watch(metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()

	listProjectsRoleName := "list-projects-role"
	if _, err := clusterAdminAuthorizationClient.ClusterRoles().Create(
		&authorizationv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: listProjectsRoleName},
			Rules: []authorizationv1.PolicyRule{
				{Verbs: []string{"list"}, APIGroups: []string{""}, Resources: []string{"projects"}},
			},
		},
	); err != nil {
		t.Fatal(err)
	}

	for _, username := range []string{"david", "jordan"} {
		projectName := username + "-project"
		if _, _, err := testserver.CreateNewProject(clusterAdminClientConfig, projectName, username); err != nil {
			t.Fatal(err)
		}
		waitForAdd(projectName, w, t)

		// make it so that the user can list projects without any groups
		if _, err := clusterAdminAuthorizationClient.ClusterRoleBindings().Create(
			&authorizationv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: username + "-clusterrolebinding"},
				Subjects: []corev1.ObjectReference{
					{Kind: authorizationapi.UserKind, Name: username},
				},
				RoleRef: corev1.ObjectReference{Name: listProjectsRoleName},
			},
		); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		name             string
		user             user.Info
		isUnauthorized   bool
		expectedProjects sets.String
	}{
		{
			name:           "empty user",
			isUnauthorized: true,
		},
		{
			name: "david can only see his project",
			user: &user.DefaultInfo{
				Name: "david",
			},
			expectedProjects: sets.NewString("david-project"),
		},
		{
			name: "david can see all projects when given cluster admin group",
			user: &user.DefaultInfo{
				Name:   "david",
				Groups: []string{"system:cluster-admins"},
			},
			expectedProjects: sets.NewString(
				"david-project",
				"jordan-project",
				"default",
				"kube-public",
				"kube-system",
				"openshift",
				"openshift-infra",
				"openshift-node",
			),
		},
	} {
		proxyHTTPHandler.setUser(test.user)

		response, err := http.Get(proxyServer.URL + "/apis/projects.openshift.io/v1/projects")
		if err != nil {
			t.Fatal(err)
		}
		data, err := ioutil.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		dataString := string(data)

		if test.isUnauthorized {
			if response.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s does not have unauthorized error: %d %s", test.name, response.StatusCode, dataString)
			}
			status := &metav1.Status{}
			if err := json.Unmarshal(data, status); err != nil {
				t.Errorf("%s failed to unmarshal status: %v %s", test.name, err, dataString)
			} else if status.Reason != metav1.StatusReasonUnauthorized || status.Code != http.StatusUnauthorized {
				t.Errorf("%s does not have unauthorized status: %#v %s", test.name, status, dataString)
			}
			continue
		}

		projectList := &projectv1.ProjectList{}
		if err := json.Unmarshal(data, projectList); err != nil {
			t.Errorf("%s failed to unmarshal project list: %v %s", test.name, err, dataString)
			continue
		}

		actualProjects := sets.NewString()
		for _, project := range projectList.Items {
			actualProjects.Insert(project.Name)
		}

		if !test.expectedProjects.Equal(actualProjects) {
			t.Errorf("%s failed to list correct projects expected %v got %v %s", test.name, test.expectedProjects.List(), actualProjects.List(), dataString)
		}
	}
}

type frontProxyHandler struct {
	proxier     *httputil.ReverseProxy
	lock        sync.Mutex
	user        user.Info
	userHeader  string
	groupHeader string
}

func (handler *frontProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Header.Del(handler.userHeader)
	r.Header.Del(handler.groupHeader)

	if handler.user != nil {
		handler.lock.Lock()
		defer handler.lock.Unlock()

		r.Header.Set(handler.userHeader, handler.user.GetName())
		for _, group := range handler.user.GetGroups() {
			r.Header.Add(handler.groupHeader, group)
		}
	}

	handler.proxier.ServeHTTP(w, r)
}

func (handler *frontProxyHandler) setUser(user user.Info) {
	handler.lock.Lock()
	defer handler.lock.Unlock()

	handler.user = user
}

func newFrontProxyHandler(rawURL, clientCA, userHeader, groupHeader string, proxyCert *tls.Certificate) (*frontProxyHandler, error) {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	rt, err := mutualAuthRoundTripper(clientCA, proxyCert)
	if err != nil {
		return nil, err
	}
	proxier := httputil.NewSingleHostReverseProxy(parsedURL)
	proxier.Transport = rt
	return &frontProxyHandler{
		proxier:     proxier,
		userHeader:  userHeader,
		groupHeader: groupHeader,
	}, nil
}

func mutualAuthRoundTripper(ca string, cert *tls.Certificate) (http.RoundTripper, error) {
	caBundleBytes, err := ioutil.ReadFile(ca)
	if err != nil {
		return nil, err
	}
	bundle := x509.NewCertPool()
	bundle.AppendCertsFromPEM(caBundleBytes)
	tlsConfig := &tls.Config{
		RootCAs:      bundle,
		ClientCAs:    bundle,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		Certificates: []tls.Certificate{*cert},
	}
	tlsConfig.BuildNameToCertificate()
	return &http.Transport{TLSClientConfig: tlsConfig}, nil
}

func createClientCert(commonName, certDir, caPrefix string) (*tls.Certificate, error) {
	signerCertOptions := &deprecatedcerts.SignerCertOptions{
		CertFile:   deprecatedcerts.DefaultCertFilename(certDir, caPrefix),
		KeyFile:    deprecatedcerts.DefaultKeyFilename(certDir, caPrefix),
		SerialFile: deprecatedcerts.DefaultSerialFilename(certDir, caPrefix),
	}
	clientCertOptions := &deprecatedcerts.CreateClientCertOptions{
		SignerCertOptions: signerCertOptions,
		CertFile:          deprecatedcerts.DefaultCertFilename(certDir, commonName),
		KeyFile:           deprecatedcerts.DefaultKeyFilename(certDir, commonName),
		ExpireDays:        crypto.DefaultCertificateLifetimeInDays,
		User:              commonName,
		Overwrite:         true,
	}
	if err := clientCertOptions.Validate(nil); err != nil {
		return nil, err
	}
	certConfig, err := clientCertOptions.CreateClientCert()
	if err != nil {
		return nil, err
	}
	certBytes, keyBytes, err := certConfig.GetPEMBytes()
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certBytes, keyBytes)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

func createCA(certDir, caPrefix string) (string, error) {
	createSignerCertOptions := deprecatedcerts.CreateSignerCertOptions{
		CertFile:   deprecatedcerts.DefaultCertFilename(certDir, caPrefix),
		KeyFile:    deprecatedcerts.DefaultKeyFilename(certDir, caPrefix),
		SerialFile: deprecatedcerts.DefaultSerialFilename(certDir, caPrefix),
		ExpireDays: crypto.DefaultCACertificateLifetimeInDays,
		Name:       caPrefix,
		Overwrite:  true,
	}
	if err := createSignerCertOptions.Validate(nil); err != nil {
		return "", err
	}
	if _, err := createSignerCertOptions.CreateSignerCert(); err != nil {
		return "", err
	}
	return createSignerCertOptions.CertFile, nil
}

func createServerCert(hostnames []string, commonName, certDir, caPrefix string) (*tls.Certificate, error) {
	signerCertOptions := &deprecatedcerts.SignerCertOptions{
		CertFile:   deprecatedcerts.DefaultCertFilename(certDir, caPrefix),
		KeyFile:    deprecatedcerts.DefaultKeyFilename(certDir, caPrefix),
		SerialFile: deprecatedcerts.DefaultSerialFilename(certDir, caPrefix),
	}
	serverCertOptions := &deprecatedcerts.CreateServerCertOptions{
		SignerCertOptions: signerCertOptions,
		CertFile:          deprecatedcerts.DefaultCertFilename(certDir, commonName),
		KeyFile:           deprecatedcerts.DefaultKeyFilename(certDir, commonName),
		ExpireDays:        crypto.DefaultCertificateLifetimeInDays,
		Hostnames:         hostnames,
		Overwrite:         true,
	}
	if err := serverCertOptions.Validate(nil); err != nil {
		return nil, err
	}
	certConfig, err := serverCertOptions.CreateServerCert()
	if err != nil {
		return nil, err
	}
	certBytes, keyBytes, err := certConfig.GetPEMBytes()
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certBytes, keyBytes)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}
