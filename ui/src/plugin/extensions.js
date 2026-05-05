/** @type {import('@openshift-console/dynamic-plugin-sdk/lib/types').Extension[]} */
const extensions = [
  {
    type: 'console.navigation/section',
    properties: {
      id: 'oc-mirror',
      name: 'OC Mirror',
      insertBefore: 'compute',
    },
  },
  {
    type: 'console.navigation/href',
    properties: {
      id: 'oc-mirror-targets',
      name: 'Mirror Targets',
      href: '/oc-mirror/targets',
      section: 'oc-mirror',
    },
  },
  {
    type: 'console.page/route',
    properties: {
      exact: true,
      path: '/oc-mirror/targets',
      component: { $codeRef: 'MirrorTargetListPage' },
    },
  },
  {
    type: 'console.page/route',
    properties: {
      exact: true,
      path: '/oc-mirror/targets/:targetName/namespaces/:namespace/imagesets/:imageSetName/catalogs/:slug',
      component: { $codeRef: 'CatalogBrowserPage' },
    },
  },
  {
    type: 'console.page/route',
    properties: {
      exact: true,
      path: '/oc-mirror/targets/:name/failures',
      component: { $codeRef: 'FailedImagesPage' },
    },
  },
  {
    type: 'console.page/route',
    properties: {
      exact: false,
      path: '/oc-mirror/targets/:name',
      component: { $codeRef: 'MirrorTargetDetailPage' },
    },
  },
];

module.exports = extensions;
