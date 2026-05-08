# UIConfiguration Examples

This directory contains example YAML manifests demonstrating all 4 exposure types for the oc-mirror dashboard UIConfiguration resource.

## Overview

The `UIConfiguration` CRD controls how the oc-mirror dashboard is exposed in your cluster. There are 4 exposure types, each suited for different deployment scenarios:

| Exposure Type | Use Case | Platform | TLS | Ingress |
|---|---|---|---|---|
| **service** | Local testing, port-forward | K8s & OpenShift | No | No |
| **ingress** | Production K8s clusters | Standard K8s | Optional | Yes (required) |
| **route** | Production OpenShift clusters | OpenShift only | Optional | No (built-in) |
| **consolePlugin** | Integrated console experience | OpenShift 4.10+ | No (handled by console) | No |

## Example Files

### 1. `ui_v1alpha1_service.yaml` - Minimal Service Exposure

**Best for:** Development, testing, port-forward access

```bash
kubectl apply -f ui_v1alpha1_service.yaml
kubectl port-forward -n oc-mirror-operator svc/oc-mirror-dashboard 3000:3000
# Access: http://localhost:3000
```

**Resources created:**
- ClusterIP Service
- Single-replica Deployment

**Key features:**
- No external access
- No TLS
- No ingress controller required

---

### 2. `ui_v1alpha1_ingress.yaml` - Full Ingress with TLS

**Best for:** Production Kubernetes clusters with ingress controllers

```bash
# 1. Create TLS secret (replace with your cert/key)
kubectl create secret tls dashboard-tls \
  --cert=path/to/cert.crt \
  --key=path/to/private.key \
  -n oc-mirror-operator

# 2. Apply the UIConfiguration
kubectl apply -f ui_v1alpha1_ingress.yaml

# 3. Verify
kubectl get ingress -n oc-mirror-operator
kubectl describe ingress oc-mirror-dashboard -n oc-mirror-operator

# 4. Update /etc/hosts or DNS
# 127.0.0.1 dashboard.example.com

# 5. Access: https://dashboard.example.com
```

**Resources created:**
- ClusterIP Service
- Kubernetes Ingress with TLS
- 2-replica Deployment with resource limits

**Key features:**
- External HTTPS access
- TLS certificate support
- Resource requests/limits specified
- Horizontal scaling (2 replicas)
- Requires ingress controller (nginx, traefik, etc.)

---

### 3. `ui_v1alpha1_route.yaml` - OpenShift Route with TLS

**Best for:** Production OpenShift/OKD clusters

```bash
# 1. Create TLS secret (replace with your cert/key)
kubectl create secret tls dashboard-tls-cert \
  --cert=path/to/cert.crt \
  --key=path/to/private.key \
  -n oc-mirror-operator

# 2. Apply the UIConfiguration
kubectl apply -f ui_v1alpha1_route.yaml

# 3. Verify
oc get routes -n oc-mirror-operator
oc describe route oc-mirror-dashboard -n oc-mirror-operator

# 4. Get the route hostname
ROUTE_HOST=$(oc get route oc-mirror-dashboard -n oc-mirror-operator \
  -o jsonpath='{.status.ingress[0].host}')
echo "Access: https://$ROUTE_HOST"
```

**Resources created:**
- ClusterIP Service
- OpenShift Route with edge TLS
- Single-replica Deployment

**Key features:**
- OpenShift-native routing
- Automatic DNS resolution via cluster domain
- TLS with edge termination
- No ingress controller required
- Better performance than Ingress on OpenShift

**Note:** Only works on OpenShift 4.x or OKD

---

### 4. `ui_v1alpha1_consoleplugin.yaml` - OpenShift Console Plugin

**Best for:** Integrated console experience on OpenShift 4.10+

```bash
# 1. Apply the UIConfiguration
kubectl apply -f ui_v1alpha1_consoleplugin.yaml

# 2. Verify plugin registration
kubectl get uiconfiguration -n oc-mirror-operator
oc get consolelugins

# 3. Monitor operator logs for plugin registration
oc logs -n oc-mirror-operator deployment/oc-mirror-operator-controller-manager -f

# 4. Access: Open OpenShift web console and look for the dashboard tab/section
# https://<openshift-console-url>
```

**Resources created:**
- ClusterIP Service (internal-only)
- Single-replica Deployment
- ConsolePlugin CR (registered with OpenShift Console)

**Key features:**
- Integrated into OpenShift web console
- No external networking required
- No TLS management needed (console handles it)
- Access controlled by OpenShift RBAC
- Most integrated user experience

**Note:** Requires OpenShift 4.10+ with Console Operator

---

### 5. `ui_v1alpha1_ingress_selfcert.yaml` - Ingress with Self-Signed Certificate (Testing)

**Best for:** Development/testing with Ingress on local clusters

```bash
# 1. Generate self-signed certificate
openssl req -x509 -newkey rsa:4096 -keyout tls.key -out tls.crt -days 365 -nodes \
  -subj "/CN=dashboard.local/O=oc-mirror/C=US"

# 2. Create secret
kubectl create secret tls dashboard-tls-selfsigned \
  --cert=tls.crt \
  --key=tls.key \
  -n oc-mirror-operator

# 3. Configure local access (Linux/Mac)
echo "127.0.0.1 dashboard.local" | sudo tee -a /etc/hosts

# 4. Apply the UIConfiguration
kubectl apply -f ui_v1alpha1_ingress_selfcert.yaml

# 5. Access (ignore certificate warnings)
# https://dashboard.local
# or via port-forward:
# kubectl port-forward -n oc-mirror-operator svc/oc-mirror-dashboard 8443:443
# https://localhost:8443
```

**Resources created:**
- ClusterIP Service
- Kubernetes Ingress with TLS (self-signed)
- Single-replica Deployment with minimal resources

**Key features:**
- No external CA required
- Good for testing ingress TLS locally
- Certificate expires in 365 days
- Self-signed certificate warnings are expected

---

## Common Tasks

### Update UIConfiguration After Creation

```bash
# Edit the UIConfiguration
kubectl edit uiconfiguration dashboard-ingress -n oc-mirror-operator

# Apply changes
kubectl apply -f ui_v1alpha1_ingress.yaml

# Monitor rollout
kubectl rollout status deployment/oc-mirror-dashboard -n oc-mirror-operator
```

### Scale Replicas

```bash
# Patch UIConfiguration to increase replicas
kubectl patch uiconfiguration dashboard-ingress -n oc-mirror-operator \
  --type merge -p '{"spec":{"replicas":3}}'

# Verify
kubectl get uiconfiguration dashboard-ingress -n oc-mirror-operator -o jsonpath='{.spec.replicas}'
```

### Check Dashboard Status

```bash
# View UIConfiguration status
kubectl get uiconfiguration -n oc-mirror-operator
kubectl describe uiconfiguration dashboard-ingress -n oc-mirror-operator

# View deployment
kubectl get deployment -n oc-mirror-operator -l app=oc-mirror-dashboard
kubectl logs -n oc-mirror-operator deployment/oc-mirror-dashboard --all-containers=true -f

# View service
kubectl get svc -n oc-mirror-operator
kubectl get endpoints -n oc-mirror-operator
```

### Troubleshooting

```bash
# Check for events
kubectl get events -n oc-mirror-operator

# Inspect UIConfiguration status
kubectl get uiconfiguration dashboard-ingress -n oc-mirror-operator -o yaml | grep -A 10 status

# Check controller logs
kubectl logs -n oc-mirror-operator deployment/oc-mirror-operator-controller-manager -f

# Test dashboard connectivity (port-forward)
kubectl port-forward -n oc-mirror-operator svc/oc-mirror-dashboard 3000:3000
curl http://localhost:3000/health  # or appropriate health check endpoint
```

## Quick Decision Tree

```
Choose your exposure type based on:

1. Are you on OpenShift 4.10+?
   ├─ YES, want console integration? → consolePlugin
   ├─ YES, need external access? → route
   └─ NO
      
2. Need external HTTPS access?
   ├─ YES → ingress
   └─ NO → service

3. Is this for production?
   ├─ YES → Use real TLS certificate (ui_v1alpha1_ingress.yaml or ui_v1alpha1_route.yaml)
   └─ NO → Use self-signed cert (ui_v1alpha1_ingress_selfcert.yaml) or service
```

## Prerequisites Checklist

### Service Exposure
- [ ] Cluster running (Kubernetes 1.20+)

### Ingress Exposure
- [ ] Ingress controller installed (nginx, traefik, etc.)
- [ ] DNS configured (or /etc/hosts entry)
- [ ] TLS certificate secret created
- [ ] IngressClass available

### Route Exposure (OpenShift only)
- [ ] Running OpenShift 4.x or OKD
- [ ] OpenShift Router running (default)
- [ ] TLS certificate secret created (optional)

### Console Plugin Exposure (OpenShift only)
- [ ] Running OpenShift 4.10+
- [ ] Console Operator running (default)
- [ ] Proper RBAC permissions

## Best Practices

1. **Start Simple:** Begin with `service` exposure for testing
2. **Use Real Certs:** Don't use self-signed certs in production
3. **Resource Limits:** Always set resource requests/limits for production
4. **High Availability:** Use 2+ replicas for production ingress/route
5. **Monitoring:** Set up monitoring and alerting for the dashboard deployment
6. **RBAC:** Use OpenShift RBAC for console plugin access control
7. **TLS Secret Rotation:** Set up certificate rotation before expiration
8. **Health Checks:** Ensure liveness/readiness probes are configured

## Related Documentation

- [UIConfiguration API Documentation](../../api/v1alpha1/uiconfiguration_types.go)
- [oc-mirror Dashboard](../../../ui/README.md)
- [Kubernetes Ingress Documentation](https://kubernetes.io/docs/concepts/services-networking/ingress/)
- [OpenShift Routes Documentation](https://docs.openshift.com/container-platform/latest/networking/routes/route-configuration.html)
- [OpenShift Console Plugins](https://docs.openshift.com/container-platform/latest/web_console/dynamic-plugins/about-dynamic-plugins.html)

## Questions or Issues?

Refer to:
- Controller logs: `kubectl logs -n oc-mirror-operator deployment/oc-mirror-operator-controller-manager`
- UIConfiguration status: `kubectl describe uiconfiguration <name> -n oc-mirror-operator`
- Events: `kubectl get events -n oc-mirror-operator`
