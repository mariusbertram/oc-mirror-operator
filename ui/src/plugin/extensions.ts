import type { Extension } from '@openshift-console/dynamic-plugin-sdk/lib/types';

const extensions: Extension[] = [
  // ── Navigation section ──────────────────────────────────────────────────────
  {
    type: 'console.navigation/section',
    properties: {
      id: 'oc-mirror',
      name: 'OC Mirror',
      insertBefore: 'compute',
    },
  },

  // ── Nav items ───────────────────────────────────────────────────────────────
  {
    type: 'console.navigation/href',
    properties: {
      id: 'oc-mirror-targets',
      name: 'Mirror Targets',
      href: '/oc-mirror/targets',
      section: 'oc-mirror',
      perspective: 'admin',
    },
  },
  {
    type: 'console.navigation/href',
    properties: {
      id: 'oc-mirror-imagesets',
      name: 'ImageSets',
      href: '/oc-mirror/imagesets',
      section: 'oc-mirror',
      perspective: 'admin',
    },
  },
  {
    type: 'console.navigation/href',
    properties: {
      id: 'oc-mirror-failed',
      name: 'Failed Images',
      href: '/oc-mirror/failed',
      section: 'oc-mirror',
      perspective: 'admin',
    },
  },

  // ── Routes ──────────────────────────────────────────────────────────────────
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
      path: '/oc-mirror/imagesets',
      component: { $codeRef: 'ImageSetListPage' },
    },
  },
  {
    type: 'console.page/route',
    properties: {
      exact: true,
      path: '/oc-mirror/failed',
      component: { $codeRef: 'FailedImagesAllPage' },
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
      exact: true,
      path: '/oc-mirror/targets/:targetName/imagesets/:imageSetName',
      component: { $codeRef: 'ImageSetDetailPage' },
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

export default extensions;
