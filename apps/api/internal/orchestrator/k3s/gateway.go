package k3s

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/sailboxhq/sailbox/apps/api/internal/model"
	"github.com/sailboxhq/sailbox/apps/api/internal/orchestrator"
)

// GVR para HTTPRoute (Gateway API v1)
var httpRouteGVR = schema.GroupVersionResource{
	Group:    "gateway.networking.k8s.io",
	Version:  "v1",
	Resource: "httproutes",
}

var gatewayGVR = schema.GroupVersionResource{
	Group:    "gateway.networking.k8s.io",
	Version:  "v1",
	Resource: "gateways",
}

var certificateGVR = schema.GroupVersionResource{
	Group:    "cert-manager.io",
	Version:  "v1",
	Resource: "certificates",
}

// CreateRoute implementa la creación de rutas usando Gateway API
func (o *Orchestrator) CreateRoute(ctx context.Context, domain *model.Domain, app *model.Application) error {
	ns := appNamespace(app)
	name := o.RouteName(app, domain.Host)

	backendPort := int32(80)
	if len(app.Ports) > 0 {
		backendPort = int32(app.Ports[0].ServicePort)
	}

	// 1. Construcción de la regla
	rule := map[string]interface{}{
		"matches": []interface{}{
			map[string]interface{}{
				"path": map[string]interface{}{"type": "PathPrefix", "value": "/"},
			},
		},
		"backendRefs": []interface{}{
			map[string]interface{}{
				"name": appK8sName(app),
				"port": backendPort,
			},
		},
	}

	// 2. Filtro HTTPS condicional
	if domain.ForceHTTPS && !isDevDomain(domain.Host) {
		rule["filters"] = []interface{}{
			map[string]interface{}{
				"type": "RequestRedirect",
				"requestRedirect": map[string]interface{}{
					"scheme":     "https",
					"statusCode": 301,
				},
			},
		}
	}

	// 2.5 Generar Certificado si es necesario
	if domain.TLS && !isDevDomain(domain.Host) {
		cert := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "cert-manager.io/v1",
				"kind":       "Certificate",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": "envoy-gateway-system", // Donde Envoy busca los certs
					"labels": map[string]interface{}{
						"app.kubernetes.io/managed-by": "sailbox",
						"sailbox/app-id":               app.ID.String(),
						"sailbox/domain-id":            domain.ID.String(),
						"sailbox/managed-cert":         "true",
					},
				},
				"spec": map[string]interface{}{
					"secretName": name + "-tls",
					"issuerRef": map[string]interface{}{
						"group": "cert-manager.io",
						"name":  "letsencrypt-prod",
						"kind":  "ClusterIssuer",
					},
					"dnsNames": []interface{}{domain.Host},
				},
			},
		}
		if err := o.applyResource(ctx, cert); err != nil {
			o.logger.Warn("Failed to create certificate resource", "error", err)
		}

		// Disparar la sincronización de certificados en el Gateway
		o.logger.Info("Syncing gateway certificates after creation", "domain", domain.Host)
		_ = o.syncGatewayCertificates(ctx)
	}

	// 3. Objeto HTTPRoute
	httpRoute := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
				"labels": map[string]interface{}{
					"app.kubernetes.io/managed-by": "sailbox",
					"sailbox/app-id":               app.ID.String(),
					"sailbox/domain-id":            domain.ID.String(),
				},
			},
			"spec": map[string]interface{}{
				"parentRefs": []interface{}{
					map[string]interface{}{
						"name":      "sailbox-gateway",
						"namespace": "envoy-gateway-system",
					},
				},
				"hostnames": []interface{}{domain.Host},
				"rules":     []interface{}{rule},
			},
		},
	}

	return o.applyResource(ctx, httpRoute)
}

// UpdateRoute es un alias de CreateRoute (Upsert)
func (o *Orchestrator) UpdateRoute(ctx context.Context, domain *model.Domain, app *model.Application) error {
	return o.CreateRoute(ctx, domain, app)
}

// DeleteRoute elimina todas las rutas asociadas a un dominio
func (o *Orchestrator) DeleteRoute(ctx context.Context, domain *model.Domain) error {
	nsList, err := o.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: "managed-by=sailbox",
	})
	if err != nil {
		return err
	}

	for _, ns := range nsList.Items {
		err := o.dynamicClient.Resource(httpRouteGVR).Namespace(ns.Name).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("sailbox/domain-id=%s", domain.ID.String()),
		})
		if err != nil && !k8serrors.IsNotFound(err) {
			o.logger.Error("failed to delete routes", "ns", ns.Name, "error", err)
		}
	}

	// Limpiar Certificados asociados en el namespace de sistema
	_ = o.dynamicClient.Resource(certificateGVR).Namespace("envoy-gateway-system").DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("sailbox/domain-id=%s", domain.ID.String()),
	})

	// Actualizar el Gateway para remover la referencia
	_ = o.syncGatewayCertificates(ctx)

	return nil
}

// DeleteRouteByName elimina una ruta específica por su nombre
func (o *Orchestrator) DeleteRouteByName(ctx context.Context, app *model.Application, name string) error {
	ns := appNamespace(app)
	err := o.dynamicClient.Resource(httpRouteGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{})

	// Intentar borrar certificado con el mismo nombre
	_ = o.dynamicClient.Resource(certificateGVR).Namespace("envoy-gateway-system").Delete(ctx, name, metav1.DeleteOptions{})

	// Sincronizar
	_ = o.syncGatewayCertificates(ctx)

	if k8serrors.IsNotFound(err) {
		return nil
	}
	return err
}

// SyncRoutePorts actualiza el puerto del backend en todas las rutas de una App
func (o *Orchestrator) SyncRoutePorts(ctx context.Context, app *model.Application) error {
	ns := appNamespace(app)
	list, err := o.dynamicClient.Resource(httpRouteGVR).Namespace(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("sailbox/app-id=%s", app.ID.String()),
	})
	if err != nil {
		return err
	}

	backendPort := int32(80)
	if len(app.Ports) > 0 {
		backendPort = int32(app.Ports[0].ServicePort)
	}

	for _, item := range list.Items {
		rules, found, _ := unstructured.NestedSlice(item.Object, "spec", "rules")
		if !found {
			continue
		}

		for _, r := range rules {
			rule := r.(map[string]interface{})
			backends, _, _ := unstructured.NestedSlice(rule, "backendRefs")
			for _, b := range backends {
				backend := b.(map[string]interface{})
				backend["port"] = backendPort
			}
		}

		err := unstructured.SetNestedSlice(item.Object, rules, "spec", "rules")
		if err == nil {
			_, _ = o.dynamicClient.Resource(httpRouteGVR).Namespace(ns).Update(ctx, &item, metav1.UpdateOptions{})
		}
	}
	return nil
}

// GetCertExpiry recupera la fecha de expiración del certificado.
func (o *Orchestrator) GetCertExpiry(ctx context.Context, domain *model.Domain, app *model.Application) (*time.Time, error) {
	name := o.RouteName(app, domain.Host)
	unstr, err := o.dynamicClient.Resource(certificateGVR).Namespace("envoy-gateway-system").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get certificate for expiry: %w", err)
	}

	// cert-manager guarda la fecha de expiración en status.notAfter
	notAfterStr, found, _ := unstructured.NestedString(unstr.Object, "status", "notAfter")
	if !found || notAfterStr == "" {
		return nil, nil
	}

	expiry, err := time.Parse(time.RFC3339, notAfterStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate expiry: %w", err)
	}

	return &expiry, nil
}

// GetRouteStatus consulta el estado real de la HTTPRoute en el clúster.
// En Gateway API, esto se verifica en el campo status.parents.
func (o *Orchestrator) GetRouteStatus(ctx context.Context, domain *model.Domain, app *model.Application) (*orchestrator.RouteStatus, error) {
	ns := appNamespace(app)
	name := o.RouteName(app, domain.Host)

	// 1. Obtener el recurso HTTPRoute de forma dinámica
	unstr, err := o.dynamicClient.Resource(httpRouteGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if k8serrors.IsNotFound(err) {
			return &orchestrator.RouteStatus{Ready: false, Message: "Route not found in cluster"}, nil
		}
		return nil, fmt.Errorf("failed to get route status: %w", err)
	}

	// 2. Analizar el status de Gateway API
	// Buscamos si la ruta ha sido aceptada por el Gateway (Accepted) y programada (Programmed)
	ready := false
	message := "Waiting for Gateway Controller..."

	// Estructura: status.parents[].conditions[]
	if parents, found, _ := unstructured.NestedSlice(unstr.Object, "status", "parents"); found && len(parents) > 0 {
		// Por simplicidad, tomamos el primer parent (nuestro sailbox-gateway)
		parent := parents[0].(map[string]interface{})
		if conditions, found, _ := unstructured.NestedSlice(parent, "conditions"); found {
			for _, cond := range conditions {
				c := cond.(map[string]interface{})
				// En Gateway API v1, "Accepted" significa que la sintaxis es correcta
				// "Programmed" significa que Envoy ya configuró el proxy
				if c["type"] == "Accepted" && c["status"] == "True" {
					ready = true
					message = "Route accepted by Gateway"
				}
			}
		}
	}

	return &orchestrator.RouteStatus{
		Ready:   ready,
		Message: message,
	}, nil
}

// --- Gestión del Panel ---

func (o *Orchestrator) EnsurePanelRoute(ctx context.Context, domain string) error {
	ns := "sailbox"
	name := "sailbox-panel"

	panelRoute := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
			},
			"spec": map[string]interface{}{
				"parentRefs": []interface{}{
					map[string]interface{}{
						"name":      "sailbox-gateway",
						"namespace": "envoy-gateway-system",
					},
				},
				"hostnames": []interface{}{domain},
				"rules": []interface{}{
					map[string]interface{}{
						"backendRefs": []interface{}{
							map[string]interface{}{
								"name": "sailbox",
								"port": 80,
							},
						},
					},
				},
			},
		},
	}

	return o.applyResource(ctx, panelRoute)
}

// syncGatewayCertificates automatiza la vinculación de secretos TLS con el Gateway global.
// Busca todos los certificados marcados por Sailbox y actualiza el Gateway.
func (o *Orchestrator) syncGatewayCertificates(ctx context.Context) error {
	// 1. Obtener el Gateway actual
	gw, err := o.dynamicClient.Resource(gatewayGVR).Namespace("envoy-gateway-system").Get(ctx, "sailbox-gateway", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get gateway for sync: %w", err)
	}

	// 2. Listar todos los certificados gestionados por Sailbox
	list, err := o.dynamicClient.Resource(certificateGVR).Namespace("envoy-gateway-system").List(ctx, metav1.ListOptions{
		LabelSelector: "sailbox/managed-cert=true",
	})
	if err != nil {
		return fmt.Errorf("failed to list managed certificates: %w", err)
	}

	// 3. Construir la lista de referencias dinámicas
	var refs []interface{}
	for _, item := range list.Items {
		// Solo añadir si el certificado está realmente listo (opcional, pero recomendado)
		secretName, found, _ := unstructured.NestedString(item.Object, "spec", "secretName")
		if found && secretName != "" {
			refs = append(refs, map[string]interface{}{"group": "", "kind": "Secret", "name": secretName})
		}
	}

	// Si no hay certificados, no podemos configurar el listener HTTPS con TLS
	if len(refs) == 0 {
		o.logger.Debug("No dynamic certificates found, skipping TLS ref update")
		return nil
	}

	// 4. Inyectar en el listener HTTPS
	listeners, found, _ := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	if !found {
		return fmt.Errorf("gateway listeners not found")
	}

	changed := false
	for i, l := range listeners {
		listener := l.(map[string]interface{})
		if listener["name"] == "https" {
			_ = unstructured.SetNestedSlice(listener, refs, "tls", "certificateRefs")
			listeners[i] = listener
			changed = true
		}
	}

	if changed {
		_ = unstructured.SetNestedSlice(gw.Object, listeners, "spec", "listeners")
		_, err = o.dynamicClient.Resource(gatewayGVR).Namespace("envoy-gateway-system").Update(ctx, gw, metav1.UpdateOptions{
			FieldManager: "sailbox-orchestrator",
		})
		return err
	}

	return nil
}

func (o *Orchestrator) DeletePanelRoute(ctx context.Context) error {
	return o.dynamicClient.Resource(httpRouteGVR).Namespace("sailbox").Delete(ctx, "sailbox-panel", metav1.DeleteOptions{})
}

// --- Helpers ---

func (o *Orchestrator) RouteName(app *model.Application, host string) string {
	return routeName(appK8sName(app), host)
}

func routeName(appName, host string) string {
	sanitizedHost := strings.ReplaceAll(strings.ReplaceAll(host, ".", "-"), "*", "wildcard")
	base := fmt.Sprintf("rt-%s-%s", appName, sanitizedHost)
	if len(base) <= 63 {
		return base
	}
	h := sha256.Sum256([]byte(appName + host))
	return fmt.Sprintf("rt-%s-%s", appName[:20], hex.EncodeToString(h[:4]))
}

// isDevDomain determina si un hostname es para desarrollo local o pruebas.
// Se usa para omitir redirecciones HTTPS forzosas y validaciones de certificados.
func isDevDomain(host string) bool {
	host = strings.ToLower(host)

	// Dominios estándar de loopback y redes locales
	if host == "localhost" ||
		strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".test") ||
		strings.HasSuffix(host, ".example") {
		return true
	}

	// Servicios de Wildcard DNS (comunes en setups de K3s/Edge)
	devServices := []string{
		"nip.io",
		"sslip.io",
		"traefik.me",
		"lvh.me",
	}

	for _, service := range devServices {
		if strings.Contains(host, service) {
			return true
		}
	}

	return false
}
