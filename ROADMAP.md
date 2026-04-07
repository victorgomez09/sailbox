# 🚀 Sailbox Evolution Roadmap

Este roadmap detalla la evolución del fork de Sailbox hacia una plataforma PaaS (Platform as a Service) de última generación, centrada en la arquitectura Kubernetes nativa, construcción de imágenes eficiente y capacidades de red avanzadas.

---

## 📍 Fase 1: Modernización del Core de Red (Gateway API)
**Objetivo:** Sustituir el Ingress tradicional por el estándar moderno de Kubernetes para habilitar multi-nodo y tráfico avanzado.

- [x] **1.1 Provisionamiento Automático de CRDs:** Implementar lógica en Go para detectar e instalar los CRDs de Gateway API (v1) al conectar un clúster.
- [-] **1.2 Despliegue de Gateway Controller:** Integrar **Envoy Gateway** o **Traefik v3** como `DaemonSet` para garantizar alta disponibilidad en todos los nodos.
- [x] **1.3 Refactorización de Rutas (HTTPRoute):** Migrar la generación de manifiestos de `Ingress` a `HTTPRoute`, vinculando dominios directamente a los `Services` (ClusterIP).
- [-] **1.4 Soporte Multi-nodo con MetalLB:** Implementar un orquestador de **IP Virtual (VIP)** mediante MetalLB para clústeres bare-metal/VPS.

---

## 🏗️ Fase 2: Motor de Build Inteligente (Zero-Config)
**Objetivo:** Eliminar la necesidad de Dockerfiles manuales y acelerar el tiempo de despliegue mediante caché compartido.

- [ ] **2.1 Integración con Nixpacks/Railpack:** Implementar el análisis automático del código fuente para detectar lenguajes y generar planes de construcción sin configuración.
- [ ] **2.2 Migración de Kaniko a Buildah:** Sustituir el motor de construcción por **Buildah** para permitir el uso de almacenamiento persistente de capas (OCI native).
- [ ] **2.3 Sistema de Caché Persistente:** Configurar un `PersistentVolumeClaim` (PVC) compartido entre los nodos de build para reducir los tiempos de compilación hasta en un 80%.
- [ ] **2.4 Registro de Imágenes Optimizado:** Integrar un registro local (Zot/Harbor) para evitar latencia de red externa en despliegues frecuentes.

---

## 🛡️ Fase 3: Resiliencia y Control de Versiones
**Objetivo:** Garantizar la estabilidad de las aplicaciones mediante historial de estado y rollbacks atómicos.

- [ ] **3.1 Instantáneas de Configuración (Snapshots):** Almacenar en PostgreSQL el estado completo de cada despliegue (Imagen + Env Vars + Recursos).
- [ ] **3.2 Rollbacks Atómicos de Un-clic:** Implementar la lógica de revertir tanto el Deployment de K8s como las variables de entorno asociadas a esa versión específica.
- [ ] **3.3 ConfigMaps Inmutables:** Generar nombres únicos con Hash para cada configuración de versión, evitando conflictos durante las transiciones de red.

---

## 💎 Fase 4: Capa Enterprise y Monetización (Features de Pago)
**Objetivo:** Funcionalidades avanzadas para usuarios profesionales y empresas que requieren control total.

- [ ] **4.1 Rate Limiting Dinámico (Pago):** - Implementar políticas de límite de peticiones (ej. 100 req/min) basadas en IP o Headers.
    - *Tecnología:* Uso de `LocalRateLimit` en Envoy Gateway.
- [ ] **4.2 Canary Deployments & Traffic Splitting (Pago):** - Interfaz visual para enviar porcentajes de tráfico (1% - 99%) entre dos versiones de la misma aplicación.
- [ ] **4.3 Soporte de Protocolos L4 (TCP/UDP) (Pago):** - Permitir el despliegue de bases de datos, brokers (Kafka/RabbitMQ) o servidores de juegos mediante `TCPRoute` y `UDPRoute`.
- [ ] **4.4 Certificados SSL Custom (BYOC):** - Opción para subir certificados propios o gestionar Wildcards avanzados mediante integración con Cert-Manager.

---

## 📊 Fase 5: Observabilidad y Analítica
**Objetivo:** Panel de control de rendimiento basado en datos reales del tráfico.

- [ ] **5.1 Métricas de Red en Tiempo Real:** Visualizar RPS (Requests por segundo), Latencia P99 y códigos de error (4xx, 5xx) por cada App.
- [ ] **5.2 Logs Centralizados de Construcción:** Sistema de streaming de logs de Buildah hacia la interfaz de usuario mediante WebSockets.
- [ ] **5.3 Alertas Inteligentes:** Notificaciones vía Webhook/Telegram cuando un despliegue falla o un límite de cuota es alcanzado.

# Esquema
                        ┌─────────────────────────────┐
                        │    DNS Wildcard              │
                        │  *.paas.empresa.com → LB IP  │
                        └──────────────┬──────────────┘
                                       │
                        ┌──────────────▼──────────────┐
                        │   Cloud/MetalLB External     │
                        │   Load Balancer (L4)         │
                        └──────────┬──────┬───────────┘
                                   │      │
               ┌───────────────────▼──┐ ┌─▼──────────────────────┐
               │  Gateway Pod - Nodo1 │ │  Gateway Pod - Nodo2    │
               │   (Envoy Gateway)    │ │   (Envoy Gateway)       │
               └───────────┬──────────┘ └──────────┬─────────────┘
                           │                        │
          ┌────────────────┴────────────────────────┘
          │
    ┌─────────▼──────────────────────────────────────────────┐
    │                  Kubernetes Cluster                      │
    │                                                          │
    │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐     │
    │  │  NS: app-a  │  │  NS: app-b  │  │  NS: app-c  │     │
    │  │  app + db   │  │  app + db   │  │  app + db   │     │
    │  └─────────────┘  └─────────────┘  └─────────────┘     │
    └──────────────────────────────────────────────────────────┘

