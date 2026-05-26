//go:build e2e

package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"

	mcpv1alpha1 "github.com/Kuadrant/mcp-gateway/api/v1alpha1"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	tlsServerName     = "mcp-tls-server"
	tlsServerPort     = int32(8443)
	tlsListenerName   = "mcps-https"
	tlsListenerHost   = "*.mcp-tls.local"
	tlsServerHostname = "tls-server.mcp-tls.local"
	caKeypairSecret   = "private-ca-keypair"
	certManagerNS     = "cert-manager"
	caLabeledSecret   = "e2e-ca-bundle"
	wrongCaSecret     = "e2e-wrong-ca"
)

var _ = Describe("Custom TLS Configuration", Ordered, func() {
	var (
		testResources    []client.Object
		mcpGatewayClient *NotifyingMCPClient
	)

	BeforeAll(func() {
		By("Checking cert-manager is installed")
		probe := &unstructured.UnstructuredList{}
		probe.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "cert-manager.io", Version: "v1", Kind: "ClusterIssuerList",
		})
		if err := k8sClient.List(ctx, probe); err != nil {
			Skip("cert-manager not installed - skipping Custom TLS tests")
		}

		By("Checking TLS test server is deployed")
		deploy := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{
			Name: tlsServerName, Namespace: TestServerNameSpace,
		}, deploy); err != nil {
			Skip("TLS test server not deployed (run 'make deploy-tls-test-server') - skipping Custom TLS tests")
		}
	})

	BeforeEach(func() {
		testResources = []client.Object{}
		Eventually(func(g Gomega) {
			var err error
			mcpGatewayClient, err = NewMCPGatewayClientWithNotifications(ctx, gatewayURL, nil)
			g.Expect(err).NotTo(HaveOccurred())
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())
	})

	AfterEach(func() {
		if mcpGatewayClient != nil {
			_ = mcpGatewayClient.Close()
			mcpGatewayClient = nil
		}
		for _, obj := range testResources {
			CleanupResource(ctx, k8sClient, obj)
		}
		testResources = []client.Object{}
	})

	It("[Happy] broker connects to TLS upstream with custom CA certificate", func() {
		By("Extracting CA cert from cert-manager secret")
		caSecret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: caKeypairSecret, Namespace: certManagerNS,
		}, caSecret)).To(Succeed())
		caCertPEM, ok := caSecret.Data["ca.crt"]
		Expect(ok).To(BeTrue(), "cert-manager CA secret should have ca.crt key")
		Expect(caCertPEM).NotTo(BeEmpty())

		By("Creating labeled CA secret in test namespace")
		labeledCA := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      caLabeledSecret,
				Namespace: TestServerNameSpace,
				Labels: map[string]string{
					"mcp.kuadrant.io/secret": "true",
					"e2e":                    "test",
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"ca.crt": caCertPEM,
			},
		}
		_ = k8sClient.Delete(ctx, labeledCA)
		Expect(k8sClient.Create(ctx, labeledCA)).To(Succeed())
		testResources = append(testResources, labeledCA)

		By("Creating MCPServerRegistration with caCertSecretRef targeting the TLS server")
		registration := NewTestResources("custom-tls", k8sClient).
			ForInternalService(tlsServerName, tlsServerPort).
			WithHostname(tlsServerHostname).
			WithPrefix("tls_e2e_").
			WithSectionName(tlsListenerName).
			WithCACertSecretRef(caLabeledSecret, "ca.crt").
			Build()
		testResources = append(testResources, registration.GetObjects()...)
		registeredServer := registration.Register(ctx)

		By("Verifying MCPServerRegistration becomes ready")
		Eventually(func(g Gomega) {
			g.Expect(VerifyMCPServerRegistrationReady(ctx, k8sClient, registeredServer.Name, registeredServer.Namespace)).To(Succeed())
		}, TestTimeoutConfigSync, TestRetryInterval).Should(Succeed())

		By("Verifying tools with tls_e2e_ prefix are present")
		Eventually(func(g Gomega) {
			toolsList, err := mcpGatewayClient.ListTools(ctx, mcpgo.ListToolsRequest{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(toolsList).NotTo(BeNil())
			g.Expect(verifyMCPServerRegistrationToolsPresent("tls_e2e_", toolsList)).To(BeTrue(),
				"tools with prefix tls_e2e_ should exist")
		}, TestTimeoutConfigSync, TestRetryInterval).Should(Succeed())
	})

	It("[Negative] broker rejects TLS upstream with wrong CA certificate", func() {
		By("Generating a wrong CA certificate")
		wrongCAPEM := generateSelfSignedCACert()

		By("Creating labeled secret with wrong CA")
		wrongCA := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      wrongCaSecret,
				Namespace: TestServerNameSpace,
				Labels: map[string]string{
					"mcp.kuadrant.io/secret": "true",
					"e2e":                    "test",
				},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				"ca.crt": wrongCAPEM,
			},
		}
		_ = k8sClient.Delete(ctx, wrongCA)
		Expect(k8sClient.Create(ctx, wrongCA)).To(Succeed())
		testResources = append(testResources, wrongCA)

		By("Creating MCPServerRegistration with wrong CA")
		registration := NewTestResources("wrong-tls", k8sClient).
			ForInternalService(tlsServerName, tlsServerPort).
			WithHostname(tlsServerHostname).
			WithPrefix("tls_wrong_").
			WithSectionName(tlsListenerName).
			WithCACertSecretRef(wrongCaSecret, "ca.crt").
			Build()
		testResources = append(testResources, registration.GetObjects()...)
		registeredServer := registration.Register(ctx)

		By("Verifying MCPServerRegistration is not ready with certificate error")
		Eventually(func(g Gomega) {
			mcpsr := &mcpv1alpha1.MCPServerRegistration{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: registeredServer.Name, Namespace: registeredServer.Namespace,
			}, mcpsr)).To(Succeed())
			g.Expect(mcpsr.Status.Conditions).NotTo(BeEmpty())
			for _, cond := range mcpsr.Status.Conditions {
				if cond.Type == "Ready" {
					g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
						"MCPServerRegistration should not be ready with wrong CA")
					g.Expect(cond.Message).To(ContainSubstring("x509"),
						"condition message should indicate a TLS certificate error")
					return
				}
			}
			g.Expect(false).To(BeTrue(), "no Ready condition found")
		}, TestTimeoutConfigSync, TestRetryInterval).Should(Succeed())

		By("Verifying tools with tls_wrong_ prefix are absent")
		Eventually(func(g Gomega) {
			toolsList, err := mcpGatewayClient.ListTools(ctx, mcpgo.ListToolsRequest{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(verifyMCPServerRegistrationToolsPresent("tls_wrong_", toolsList)).To(BeFalse(),
				"tools with prefix tls_wrong_ should NOT exist")
		}, TestTimeoutMedium, TestRetryInterval).Should(Succeed())
	})
})

func generateSelfSignedCACert() []byte {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Wrong CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	Expect(err).NotTo(HaveOccurred())
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
