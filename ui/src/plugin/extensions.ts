import type { Extension } from '@openshift-console/dynamic-plugin-sdk/lib/types';

const extensions: Extension[] = [
  {
    type: 'console.navigation/section',
    properties: {
      id: 'oc-mirror',
      name: 'OC Mirror',
      insertBefore: 'compute',
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
      exact: false,
      path: '/oc-mirror/targets/:name',
      component: { $codeRef: 'MirrorTargetDetailPage' },
    },
  },
  {
    type: 'console.page/route',
    properties: {
      exact: true,
      path: '/oc-mirror/targets/:targetName/catalogs/:slug',
      component: { $codeRef: 'CatalogBrowserPage' },
    },
  },
];

export default extensions;
