// Real Kubernetes resources used by both local and CI browser validation.
const namespace = 'oc-mirror-demo';
const catalog = 'registry.example.com/demo/index:v1';
const slug = 'index-v1';
const metadata = (name, labels) => ({ name, namespace, ...(labels ? { labels } : {}) });
const status = {
  totalImages: 42, mirroredImages: 30, pendingImages: 10, failedImages: 2,
  imageSetStatuses: [{ name: 'demo-imageset', found: true, total: 42, mirrored: 30, pending: 10, failed: 2 }],
  conditions: [{ type: 'Ready', status: 'False', reason: 'Mirroring', message: 'Smoke fixture is mirroring', lastTransitionTime: '2026-01-01T00:00:00Z' }],
};
const packages = {
  catalog, targetImage: 'registry.example.com/mirror/index:v1',
  packages: [{
    name: 'demo-operator', defaultChannel: 'stable',
    channels: [{ name: 'stable', versions: ['1.0.0', '1.1.0'] }],
  }],
};
const resources = [
  {
    apiVersion: 'mirror.openshift.io/v1alpha1', kind: 'ImageSet', metadata: metadata('demo-imageset'),
    spec: { mirror: {
      additionalImages: [{ name: 'registry.example.com/demo/pause:latest' }],
      operators: [{ catalog, packages: [{ name: 'demo-operator' }] }],
      platform: { channels: [{ name: 'stable-4.18', type: 'ocp', minVersion: '4.18.1' }] },
    } },
  },
  {
    apiVersion: 'mirror.openshift.io/v1alpha1', kind: 'MirrorTarget', metadata: metadata('demo-target'),
    spec: { registry: 'registry.example.com/mirror', imageSets: ['demo-imageset'] },
  },
  {
    apiVersion: 'v1', kind: 'ConfigMap', metadata: metadata(`oc-mirror-demo-target-${slug}-packages`, {
      'oc-mirror.openshift.io/mirrortarget': 'demo-target',
      'oc-mirror.openshift.io/catalog-packages': slug,
    }),
    data: { 'packages.json': JSON.stringify(packages) },
  },
  {
    apiVersion: 'v1', kind: 'ConfigMap', metadata: metadata(`oc-mirror-demo-target-${slug}-upstream-packages`),
    data: { 'packages.json': JSON.stringify(packages) },
  },
  {
    apiVersion: 'v1', kind: 'ConfigMap', metadata: metadata('oc-mirror-demo-target-resources'),
    data: { 'idms.yaml': 'apiVersion: config.openshift.io/v1\nkind: ImageDigestMirrorSet\nmetadata:\n  name: demo-mirrors\nspec:\n  imageDigestMirrors: []\n' },
  },
  {
    apiVersion: 'v1', kind: 'ConfigMap', metadata: metadata('demo-imageset-images'),
    data: { 'images.json': JSON.stringify({
      'registry.example.com/mirror/pause:latest': {
        source: 'registry.example.com/demo/pause:latest', state: 'Failed',
        retryCount: 2, lastError: 'Smoke fixture registry unavailable', origin: 'additional',
      },
    }) },
  },
];
console.log(JSON.stringify(process.argv.includes('--status') ? { status } : { apiVersion: 'v1', kind: 'List', items: resources }));
