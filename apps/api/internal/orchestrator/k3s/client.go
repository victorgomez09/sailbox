package k3s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/sailboxhq/sailbox/apps/api/internal/config"
	"github.com/sailboxhq/sailbox/apps/api/internal/orchestrator"
	"github.com/sailboxhq/sailbox/apps/api/manifests"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// Orchestrator implements orchestrator.Orchestrator with real K3s/K8s API calls.
type Orchestrator struct {
	client        kubernetes.Interface
	dynamicClient dynamic.Interface
	mapper        meta.RESTMapper
	config        *rest.Config
	logger        *slog.Logger
}

// Compile-time check that Orchestrator implements the interface.
var _ orchestrator.Orchestrator = (*Orchestrator)(nil)

// New creates a K3s orchestrator connected to a real cluster.
func New(cfg config.K8sConfig, logger *slog.Logger) (*Orchestrator, error) {
	var restCfg *rest.Config
	var err error

	if cfg.InCluster {
		restCfg, err = rest.InClusterConfig()
	} else if cfg.Kubeconfig != "" {
		restCfg, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	} else {
		// Try default kubeconfig locations
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		configOverrides := &clientcmd.ConfigOverrides{}
		restCfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides).ClientConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to build k8s config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	discoveryClient := clientset.Discovery()
	groupResources, err := restmapper.GetAPIGroupResources(discoveryClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get api group resources: %w", err)
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)

	// Verify connection
	_, err = clientset.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to k8s cluster: %w", err)
	}

	orch := &Orchestrator{
		client:        clientset,
		dynamicClient: dynamicClient,
		mapper:        mapper,
		config:        restCfg,
		logger:        logger,
	}

	if err := orch.ensureGatewayAPI(); err != nil {
		return nil, fmt.Errorf("failed to ensure gateway api: %w", err)
	}
	if err := orch.ensureEnvoyGateway(); err != nil {
		return nil, fmt.Errorf("failed to ensure envoy: %w", err)
	}

	// 1. Configurar HA (DaemonSet) ANTES de crear el Gateway
	if err := orch.configureHighAvailability(); err != nil {
		orch.logger.Warn("No se pudo aplicar la configuración de HA, usando valores por defecto", "error", err)
	}

	// 2. Crear el Gateway (ahora ya sabe que debe ser DaemonSet)
	if err := orch.ensureGlobalGateway(); err != nil {
		return nil, fmt.Errorf("failed to ensure HA gateway: %w", err)
	}

	// 3. Asegurar MetalLB para la IP Virtual
	if err := orch.EnsureMetalLB(context.Background()); err != nil {
		logger.Warn("Failed to ensure MetalLB installation", "error", err)
	}

	logger.Info("connected to K3s cluster with Gateway API ready")
	return orch, nil
}

func (o *Orchestrator) ensureGatewayAPI() error {
	o.logger.Info("Checking Gateway API status...")

	// 1. Verificar si el grupo ya existe en el Discovery
	groups, err := o.client.Discovery().ServerGroups()
	if err != nil {
		return fmt.Errorf("failed to get server groups: %w", err)
	}

	apiPresent := false
	for _, g := range groups.Groups {
		if g.Name == "gateway.networking.k8s.io" {
			apiPresent = true
			break
		}
	}

	if apiPresent {
		o.logger.Info("Gateway API is already installed")
		return nil
	}

	o.logger.Warn("Gateway API not found. Starting production-ready installation...")

	// 2. Parsear el YAML embebido
	// El manifiesto estándar contiene múltiples recursos (CRDs) separados por ---
	reader := strings.NewReader(manifests.GatewayAPI)
	decoder := yaml.NewYAMLOrJSONDecoder(reader, 4096)

	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(obj)

		if err == io.EOF {
			break // Terminamos de leer el manifiesto
		}
		if err != nil {
			return fmt.Errorf("failed to decode Gateway API manifest: %w", err)
		}

		if obj == nil || obj.Object == nil {
			continue
		}

		if err := o.applyResource(context.Background(), obj); err != nil {
			return fmt.Errorf("failed to apply Gateway API resource %s: %w", obj.GetName(), err)
		}
	}

	o.logger.Info("Waiting for CRDs to be ready...")
	time.Sleep(5 * time.Second)

	return nil
}

// applyResource aplica un recurso arbitrario al clúster (equivalente a kubectl apply)
func (o *Orchestrator) applyResource(ctx context.Context, obj *unstructured.Unstructured) error {
	// 1. Obtener el GVK (Group, Version, Kind)
	gvk := obj.GroupVersionKind()

	// 2. Obtener el mapeo REST para saber si es Namespaced o no
	mapping, err := o.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("failed to get rest mapping: %w", err)
	}

	var dr dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		// RECURSOS CON NAMESPACE (como ServiceAccount)
		ns := obj.GetNamespace()
		if ns == "" {
			ns = "envoy-gateway-system" // Fallback si el YAML no lo trae
		}
		dr = o.dynamicClient.Resource(mapping.Resource).Namespace(ns)
	} else {
		// RECURSOS DE CLUSTER (como ClusterRole)
		dr = o.dynamicClient.Resource(mapping.Resource)
	}

	// 3. Ejecutar el Create o Update
	data, err := obj.MarshalJSON()
	if err != nil {
		return err
	}

	o.logger.Debug("Applying resource", "kind", gvk.Kind, "name", obj.GetName())

	_, err = dr.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
		FieldManager: "sailbox",
		Force:        ptr.To(true),
	})

	return err
}

func (o *Orchestrator) ensureEnvoyGateway() error {
	o.logger.Info("Installing Envoy Gateway Controller...")

	// 1. Aplicar el manifiesto base (Controller + RBAC + Services)
	if err := o.applyManifest(manifests.EnvoyGateway); err != nil {
		return fmt.Errorf("failed to apply envoy gateway manifest: %w", err)
	}

	// 2. Definir la GatewayClass (El "enlace" lógico)
	// Esto le dice a Kubernetes: "Cuando veas un Gateway de clase 'envoy', usa Envoy Gateway"
	gatewayClass := `apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: sailbox-gateway-class
spec:
  controllerName: gateway.envoyproxy.io/gatewayclass-controller
  parametersRef:
    group: gateway.envoyproxy.io
    kind: EnvoyProxy
    name: sailbox-proxy-config
    namespace: envoy-gateway-system
`
	if err := o.applyManifest(gatewayClass); err != nil {
		return fmt.Errorf("failed to apply GatewayClass: %w", err)
	}

	o.logger.Info("Envoy Gateway stack deployed successfully")
	return nil
}

// applyManifest es una función helper que reutiliza la lógica de parseo de YAML
func (o *Orchestrator) applyManifest(manifest string) error {
	// Convertimos el string a un Reader para el decoder de K8s
	reader := bytes.NewReader([]byte(manifest))
	decoder := yaml.NewYAMLOrJSONDecoder(reader, 4096)

	for {
		obj := &unstructured.Unstructured{}
		err := decoder.Decode(obj)

		// Si llegamos al final del "archivo/string", salimos sin error
		if err == io.EOF {
			break
		}

		if err != nil {
			return fmt.Errorf("error decoding manifest part: %w", err)
		}

		// Si el documento está vacío (ej. solo comentarios o separadores extra),
		// el objeto no tendrá contenido. Esto evita el error "Kind is missing in null".
		if obj.Object == nil || len(obj.Object) == 0 {
			continue
		}

		// Aseguramos que tenga un Kind antes de enviarlo a applyResource
		if obj.GetKind() == "" {
			o.logger.Debug("skipping manifest part without Kind")
			continue
		}

		// Aplicamos el recurso
		if err := o.applyResource(context.Background(), obj); err != nil {
			return fmt.Errorf("failed to apply resource %s/%s: %w",
				obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

func (o *Orchestrator) configureHighAvailability() error {
	// Este recurso le dice a Envoy Gateway cómo queremos que sean los proxies físicos
	haConfig := `apiVersion: gateway.envoyproxy.io/v1alpha1
kind: EnvoyProxy
metadata:
  name: sailbox-proxy-config
  namespace: envoy-gateway-system
spec:
  provider:
    type: Kubernetes
    kubernetes:
      envoyProxy:
        kind: DaemonSet
      envoyDaemonSet:
        pod:
          spec:
            hostNetwork: true
            dnsPolicy: ClusterFirstWithHostNet
            tolerations:
              - key: node-role.kubernetes.io/control-plane
                operator: Exists
                effect: NoSchedule
              - key: node-role.kubernetes.io/master
                operator: Exists
                effect: NoSchedule
        container:
          securityContext:
            capabilities:
              add:
                - NET_BIND_SERVICE
            runAsUser: 0
      envoyService:
        type: LoadBalancer
        externalTrafficPolicy: Local
`
	return o.applyManifest(haConfig)
}

func (o *Orchestrator) ensureGlobalGateway() error {
	o.logger.Info("Creando el punto de entrada global (Gateway)...")

	// Este es el objeto que 'levanta' los puertos 80/443 en todos los nodos
	gatewayYAML := `
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: sailbox-gateway
  namespace: envoy-gateway-system
spec:
  gatewayClassName: sailbox-gateway-class
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: All
    - name: https
      protocol: HTTPS
      port: 443
      allowedRoutes:
        namespaces:
          from: All
      tls:
        mode: Terminate
`
	return o.applyManifest(gatewayYAML)
}

func (o *Orchestrator) EnsureClusterIssuer(ctx context.Context, email string, cloudflareToken string) error {
	if email == "" {
		return nil
	}

	var solvers string
	if cloudflareToken != "" {
		// Crear el secreto para cert-manager
		secret := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Secret",
				"metadata": map[string]interface{}{
					"name":      "cloudflare-api-token",
					"namespace": "envoy-gateway-system",
				},
				"stringData": map[string]interface{}{
					"api-token": cloudflareToken,
				},
			},
		}
		_ = o.applyResource(ctx, secret)

		solvers = `
    - dns01:
        cloudflare:
          apiTokenSecretRef:
            name: cloudflare-api-token
            key: api-token`
	} else {
		solvers = `
    - http01:
        gatewayHTTPRoute:
          parentRefs:
          - name: sailbox-gateway
            namespace: envoy-gateway-system`
	}

	issuerYAML := fmt.Sprintf(`
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: %s
    privateKeySecretRef:
      name: letsencrypt-prod
    solvers:
%s
`, email, solvers)

	o.logger.Info("Configuring ClusterIssuer", "email", email, "mode", "dns01/http01")
	return o.applyManifest(issuerYAML)
}

func (o *Orchestrator) EnsureMetalLB(ctx context.Context) error {
	o.logger.Info("Checking MetalLB status...")

	// Comprobar si el namespace ya existe como proxy de instalación
	_, err := o.client.CoreV1().Namespaces().Get(ctx, "metallb-system", metav1.GetOptions{})
	if err == nil {
		o.logger.Info("MetalLB is already installed")
		return nil
	}

	o.logger.Info("Installing MetalLB for multi-node support...")
	// Aquí se aplicaría el manifiesto de instalación oficial de MetalLB
	// Por simplicidad en este ejemplo, asumimos que está en manifests.MetalLB
	if err := o.applyManifest(manifests.MetalLB); err != nil {
		return fmt.Errorf("failed to install metallb: %w", err)
	}

	return nil
}

func (o *Orchestrator) ConfigureMetalLB(ctx context.Context, ipRange string) error {
	if ipRange == "" {
		return nil
	}

	o.logger.Info("Configuring MetalLB IP Pool", "range", ipRange)

	poolConfig := fmt.Sprintf(`
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: sailbox-pool
  namespace: metallb-system
spec:
  addresses:
  - %s
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: sailbox-l2
  namespace: metallb-system
spec:
  ipAddressPools:
  - sailbox-pool
`, ipRange)

	return o.applyManifest(poolConfig)
}

func (o *Orchestrator) GetSuggestedIPRange(ctx context.Context) (string, error) {
	nodes, err := o.GetNodes(ctx)
	if err != nil || len(nodes) == 0 {
		return "", fmt.Errorf("failed to get nodes for network detection: %w", err)
	}

	// Usamos la IP del primer nodo como referencia de la subred de gestión.
	nodeIP := net.ParseIP(nodes[0].IP).To4()
	if nodeIP == nil {
		return "", fmt.Errorf("node IP %s is not a valid IPv4 address", nodes[0].IP)
	}

	// Sugerimos un rango al final de la subred /24 (ej: .200-.250).
	// Esto minimiza colisiones con IPs de nodos o pools DHCP iniciales.
	prefix := fmt.Sprintf("%d.%d.%d", nodeIP[0], nodeIP[1], nodeIP[2])
	return fmt.Sprintf("%s.200-%s.250", prefix, prefix), nil
}
