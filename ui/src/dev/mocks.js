/**
 * Mock data for the Console Plugin dev harness.
 * Activated via:  MOCK=true npm --prefix ui run dev
 *
 * Registers Express handlers on the webpack dev server app before the proxy
 * middleware, so no running Go backend or cluster is needed.
 */

// ── Helpers ────────────────────────────────────────────────────────────────

function json(res, data) {
  res.setHeader('Content-Type', 'application/json');
  res.end(JSON.stringify(data));
}

function yaml(res, text) {
  res.setHeader('Content-Type', 'application/yaml');
  res.end(text);
}

// ── Shared data ────────────────────────────────────────────────────────────

const TARGETS_LIST = [
  {
    namespace: 'oc-mirror-operator',
    name: 'production',
    registry: 'registry.example.com/oc-mirror',
    totalImages: 1247,
    mirroredImages: 1089,
    pendingImages: 143,
    failedImages: 15,
  },
  {
    namespace: 'oc-mirror-operator',
    name: 'staging',
    registry: 'registry.staging.example.com/oc-mirror',
    totalImages: 432,
    mirroredImages: 432,
    pendingImages: 0,
    failedImages: 0,
  },
];

const CATALOG_SLUGS = {
  production: [
    {
      slug: 'redhat-operators-v4-14',
      source: 'registry.redhat.io/redhat/redhat-operator-index:v4.14',
      targetImage: 'registry.example.com/oc-mirror/redhat/redhat-operator-index:v4.14',
      filteredPackagesUrl: '/api/v1/targets/production/catalogs/redhat-operators-v4-14/packages.json',
      upstreamPackagesUrl: '/api/v1/targets/production/catalogs/redhat-operators-v4-14/upstream-packages.json',
    },
    {
      slug: 'certified-operators-v4-14',
      source: 'registry.redhat.io/redhat/certified-operator-index:v4.14',
      targetImage: 'registry.example.com/oc-mirror/redhat/certified-operator-index:v4.14',
      filteredPackagesUrl: '/api/v1/targets/production/catalogs/certified-operators-v4-14/packages.json',
      upstreamPackagesUrl: '/api/v1/targets/production/catalogs/certified-operators-v4-14/upstream-packages.json',
    },
  ],
};

const TARGET_DETAILS = {
  production: {
    ...TARGETS_LIST[0],
    conditions: [
      { type: 'Ready', status: 'True', reason: 'ReconcileSuccess', message: 'All resources reconciled.' },
      {
        type: 'Progressing',
        status: 'True',
        reason: 'MirroringInProgress',
        message: 'Mirroring 143 pending images across 2 ImageSets.',
      },
    ],
    imageSets: [
      {
        name: 'release-4-14',
        found: true,
        total: 890,
        mirrored: 782,
        pending: 108,
        failed: 0,
        resources: [
          {
            name: 'IDMS',
            url: '/api/v1/targets/production/imagesets/release-4-14/idms.yaml',
            type: 'idms',
          },
          {
            name: 'ITMS',
            url: '/api/v1/targets/production/imagesets/release-4-14/itms.yaml',
            type: 'itms',
          },
        ],
      },
      {
        name: 'operators-stable',
        found: true,
        total: 357,
        mirrored: 307,
        pending: 35,
        failed: 15,
        resources: [
          {
            name: 'IDMS',
            url: '/api/v1/targets/production/imagesets/operators-stable/idms.yaml',
            type: 'idms',
          },
          {
            name: 'CatalogSource (redhat-operators)',
            url: '/api/v1/targets/production/imagesets/operators-stable/catalogs/redhat-operators-v4-14/catalogsource.yaml',
            type: 'catalogsource',
          },
          {
            name: 'CatalogSource (certified-operators)',
            url: '/api/v1/targets/production/imagesets/operators-stable/catalogs/certified-operators-v4-14/catalogsource.yaml',
            type: 'catalogsource',
          },
        ],
      },
    ],
    resources: [],
    catalogs: CATALOG_SLUGS.production,
  },
  staging: {
    ...TARGETS_LIST[1],
    conditions: [
      {
        type: 'Ready',
        status: 'True',
        reason: 'ReconcileSuccess',
        message: 'All 432 images mirrored successfully.',
      },
    ],
    imageSets: [
      {
        name: 'release-4-13',
        found: true,
        total: 432,
        mirrored: 432,
        pending: 0,
        failed: 0,
        resources: [
          {
            name: 'IDMS',
            url: '/api/v1/targets/staging/imagesets/release-4-13/idms.yaml',
            type: 'idms',
          },
          {
            name: 'ITMS',
            url: '/api/v1/targets/staging/imagesets/release-4-13/itms.yaml',
            type: 'itms',
          },
        ],
      },
    ],
    resources: [],
    catalogs: [],
  },
};

const IMAGE_FAILURES = {
  production: {
    failed: [
      {
        destination: 'registry.example.com/oc-mirror/openshift/release:4.14.12-x86_64-cluster-etcd-operator',
        source: 'quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:abc123',
        state: 'Failed',
        lastError: 'dial tcp: connection refused',
        retryCount: 10,
        permanentlyFailed: true,
        imageSet: 'release-4-14',
      },
      {
        destination: 'registry.example.com/oc-mirror/openshift/release:4.14.12-x86_64-aws-ebs-csi-driver',
        source: 'quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:def456',
        state: 'Failed',
        lastError: 'unexpected EOF reading manifest',
        retryCount: 7,
        permanentlyFailed: false,
        imageSet: 'release-4-14',
      },
      {
        destination: 'registry.example.com/oc-mirror/amq7/amq-broker-rhel8-operator@sha256:111aaa',
        source: 'registry.redhat.io/amq7/amq-broker-rhel8-operator@sha256:111aaa',
        state: 'Failed',
        lastError: 'unauthorized: Please login to the Red Hat Registry',
        retryCount: 10,
        permanentlyFailed: true,
        imageSet: 'operators-stable',
      },
    ],
    pending: [
      {
        destination: 'registry.example.com/oc-mirror/openshift/release:4.14.12-x86_64-machine-config-operator',
        source: 'quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:fed789',
        state: 'Pending',
        retryCount: 0,
        imageSet: 'release-4-14',
      },
      {
        destination: 'registry.example.com/oc-mirror/openshift/release:4.14.12-x86_64-kube-apiserver',
        source: 'quay.io/openshift-release-dev/ocp-v4.0-art-dev@sha256:abc999',
        state: 'Pending',
        retryCount: 0,
        imageSet: 'release-4-14',
      },
    ],
  },
  staging: { failed: [], pending: [] },
};

// ── Catalog packages ────────────────────────────────────────────────────────

const FILTERED_PACKAGES = {
  catalog: 'redhat-operators-v4-14',
  targetImage: 'registry.example.com/oc-mirror/redhat/redhat-operator-index:v4.14',
  packages: [
    {
      name: 'amq-broker-rhel8',
      defaultChannel: 'eus-2.6.x',
      description: 'AMQ Broker is a full-featured, message-oriented middleware broker.',
      channels: [
        {
          name: 'eus-2.6.x',
          versions: ['7.11.0', '7.11.1', '7.11.2', '7.11.3'],
          entries: [
            { name: 'amq-broker-operator.v7.11.0', version: '7.11.0' },
            { name: 'amq-broker-operator.v7.11.3', version: '7.11.3' },
          ],
        },
      ],
    },
    {
      name: 'cluster-logging',
      defaultChannel: 'stable-6.0',
      description: 'The Red Hat OpenShift Logging Operator.',
      channels: [
        {
          name: 'stable-6.0',
          versions: ['6.0.0', '6.0.1', '6.0.2'],
          entries: [
            { name: 'cluster-logging.v6.0.0', version: '6.0.0' },
            { name: 'cluster-logging.v6.0.2', version: '6.0.2' },
          ],
        },
        {
          name: 'stable-5.9',
          versions: ['5.9.0', '5.9.1'],
          entries: [
            { name: 'cluster-logging.v5.9.0', version: '5.9.0' },
            { name: 'cluster-logging.v5.9.1', version: '5.9.1' },
          ],
        },
      ],
    },
  ],
};

const UPSTREAM_PACKAGES = {
  catalog: 'redhat-operators-v4-14',
  targetImage: 'registry.example.com/oc-mirror/redhat/redhat-operator-index:v4.14',
  packages: [
    ...FILTERED_PACKAGES.packages,
    {
      name: 'elasticsearch-operator',
      defaultChannel: 'eus-5.8.x',
      description: 'Elasticsearch Operator for OpenShift.',
      channels: [
        {
          name: 'eus-5.8.x',
          versions: ['5.8.0', '5.8.1', '5.8.2'],
          entries: [
            { name: 'elasticsearch-operator.v5.8.0', version: '5.8.0' },
            { name: 'elasticsearch-operator.v5.8.2', version: '5.8.2' },
          ],
        },
      ],
    },
    {
      name: 'jaeger-product',
      defaultChannel: 'stable',
      description: 'Jaeger distributed tracing system.',
      channels: [
        {
          name: 'stable',
          versions: ['1.57.0', '1.58.0'],
          entries: [
            { name: 'jaeger-operator.v1.57.0', version: '1.57.0' },
            { name: 'jaeger-operator.v1.58.0', version: '1.58.0' },
          ],
        },
      ],
    },
    {
      name: 'servicemeshoperator',
      defaultChannel: 'stable',
      description: 'Red Hat OpenShift Service Mesh Operator.',
      channels: [
        {
          name: 'stable',
          versions: ['2.5.0', '2.5.1', '2.6.0'],
          entries: [
            { name: 'servicemeshoperator.v2.5.0', version: '2.5.0' },
            { name: 'servicemeshoperator.v2.6.0', version: '2.6.0' },
          ],
        },
      ],
    },
  ],
};

const PACKAGE_CONSTRAINTS = [
  {
    name: 'amq-broker-rhel8',
    minVersion: '7.11.0',
    maxVersion: '7.11.3',
    channels: [{ name: 'eus-2.6.x' }],
  },
  {
    name: 'cluster-logging',
    channels: [{ name: 'stable-6.0', minVersion: '6.0.0' }],
  },
];

// ── Raw YAML stubs ─────────────────────────────────────────────────────────

const IDMS_YAML = `\
apiVersion: config.openshift.io/v1
kind: ImageDigestMirrorSet
metadata:
  name: oc-mirror-production
spec:
  imageDigestMirrors:
    - mirrors:
        - registry.example.com/oc-mirror/openshift/release
      source: quay.io/openshift-release-dev/ocp-v4.0-art-dev
    - mirrors:
        - registry.example.com/oc-mirror/openshift-release-dev/ocp-release
      source: quay.io/openshift-release-dev/ocp-release
`;

const ITMS_YAML = `\
apiVersion: config.openshift.io/v1
kind: ImageTagMirrorSet
metadata:
  name: oc-mirror-production
spec:
  imageTagMirrors:
    - mirrors:
        - registry.example.com/oc-mirror/ubi9
      source: registry.access.redhat.com/ubi9
`;

const CATALOGSOURCE_YAML = `\
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: oc-mirror-redhat-operators
  namespace: openshift-marketplace
spec:
  sourceType: grpc
  image: registry.example.com/oc-mirror/redhat/redhat-operator-index:v4.14
  displayName: Red Hat Operators (mirrored)
  publisher: Red Hat
`;

// ── Register handlers ──────────────────────────────────────────────────────

/**
 * registerMockHandlers adds all mock API routes to the webpack dev server's
 * Express app.  These are inserted before the proxy middleware so they take
 * priority regardless of whether a real backend is running.
 *
 * @param {import('express').Application} app
 */
function registerMockHandlers(app) {
  console.log('[mock] Registering mock API handlers (MOCK=true)');

  app.get('/api/v1/targets', (_req, res) => json(res, TARGETS_LIST));

  app.get('/api/v1/targets/:mt', (req, res) => {
    const detail = TARGET_DETAILS[req.params.mt];
    if (!detail) return res.status(404).end();
    json(res, detail);
  });

  app.get('/api/v1/targets/:mt/image-failures', (req, res) => {
    const failures = IMAGE_FAILURES[req.params.mt] ?? { failed: [], pending: [] };
    json(res, failures);
  });

  // Catalog packages (filtered = what is already mirrored)
  app.get('/api/v1/targets/:mt/catalogs/:slug/packages.json', (_req, res) =>
    json(res, FILTERED_PACKAGES),
  );

  // Upstream packages (full catalog)
  app.get('/api/v1/targets/:mt/catalogs/:slug/upstream-packages.json', (_req, res) =>
    json(res, UPSTREAM_PACKAGES),
  );

  // Package constraints (what the ImageSet spec defines)
  app.get('/api/v1/imagesets/:namespace/:name/catalogs/:slug/packages', (_req, res) =>
    json(res, PACKAGE_CONSTRAINTS),
  );

  // PATCH recollect — accept and respond 204
  app.patch('/api/v1/imagesets/:namespace/:name/recollect', (_req, res) =>
    res.status(204).end(),
  );

  // PATCH catalog packages — accept and respond 204
  app.patch('/api/v1/imagesets/:namespace/:name/catalogs/:slug/packages', (_req, res) =>
    res.status(204).end(),
  );

  // DELETE imageset — accept and respond 204
  app.delete('/api/v1/imagesets/:namespace/:name', (_req, res) =>
    res.status(204).end(),
  );

  // Raw YAML resources
  app.get('/api/v1/targets/:mt/imagesets/:is/idms.yaml', (_req, res) =>
    yaml(res, IDMS_YAML),
  );
  app.get('/api/v1/targets/:mt/imagesets/:is/itms.yaml', (_req, res) =>
    yaml(res, ITMS_YAML),
  );
  app.get('/api/v1/targets/:mt/imagesets/:is/catalogs/:slug/catalogsource.yaml', (_req, res) =>
    yaml(res, CATALOGSOURCE_YAML),
  );
  app.get('/api/v1/targets/:mt/imagesets/:is/catalogs/:slug/clustercatalog.yaml', (_req, res) =>
    yaml(res, CATALOGSOURCE_YAML),
  );
}

module.exports = { registerMockHandlers };
