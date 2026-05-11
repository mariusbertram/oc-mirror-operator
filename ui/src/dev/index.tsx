/**
 * Standalone dev harness — runs the Console Plugin pages without an OpenShift Console.
 *
 * Start with:  npm --prefix ui run dev
 *
 * The webpack dev server (port 9002) proxies /api/ requests to the local Go plugin
 * backend at https://localhost:9443.  No consoleFetch or ConsolePlugin SDK needed.
 */
import React from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter, Navigate, NavLink, Route, Routes } from 'react-router-dom';

// PatternFly base CSS — provides global custom properties for colour, spacing, typography.
import '@patternfly/react-core/dist/styles/base.css';

import { MirrorTargetList } from '../pages/MirrorTargets/MirrorTargetList';
import { MirrorTargetDetail } from '../pages/MirrorTargets/MirrorTargetDetail';
import { FailedImages } from '../pages/MirrorTargets/FailedImages';
import { ImageSetList } from '../pages/ImageSets/ImageSetList';
import { ImageSetDetail } from '../pages/ImageSets/ImageSetDetail';
import { CatalogBrowser } from '../pages/CatalogBrowser/CatalogBrowser';
import { ReleaseBrowser } from '../pages/ReleaseBrowser/ReleaseBrowser';

// The default API client uses window.fetch with an empty base URL, so all /api/v1/...
// requests go to the same origin — the webpack dev server proxies them to :9443.
// No setFetchImpl / setApiBaseUrl overrides are needed here.

function navStyle({ isActive }: { isActive: boolean }): React.CSSProperties {
  return {
    display: 'block',
    padding: '8px 16px',
    color: isActive ? '#73bcf7' : '#d2d2d2',
    textDecoration: 'none',
    fontSize: 14,
    background: isActive ? 'rgba(0,102,204,0.15)' : 'transparent',
  };
}

function App() {
  return (
    <BrowserRouter>
      <div style={{ display: 'flex', height: '100vh' }}>
        <nav style={{ width: 200, background: '#212427', padding: '16px 0', flexShrink: 0 }}>
          <div style={{ padding: '8px 16px 20px', color: '#fff', fontWeight: 700, fontSize: 15 }}>
            OC Mirror — Dev
          </div>
          <NavLink to="/oc-mirror/targets" style={navStyle} end>
            Mirror Targets
          </NavLink>
          <NavLink to="/oc-mirror/imagesets" style={navStyle}>
            ImageSets
          </NavLink>
          <NavLink to="/oc-mirror/failed" style={navStyle}>
            Failed Images
          </NavLink>
        </nav>
        <main style={{ flex: 1, overflow: 'auto' }}>
          <Routes>
            <Route path="/" element={<Navigate to="/oc-mirror/targets" replace />} />
            <Route path="/oc-mirror/targets" element={<MirrorTargetList />} />
            <Route path="/oc-mirror/targets/:name/failures" element={<FailedImages />} />
            <Route
              path="/oc-mirror/targets/:targetName/namespaces/:namespace/imagesets/:imageSetName/releases"
              element={<ReleaseBrowser />}
            />
            <Route
              path="/oc-mirror/targets/:targetName/namespaces/:namespace/imagesets/:imageSetName/catalogs/:slug"
              element={<CatalogBrowser />}
            />
            <Route
              path="/oc-mirror/targets/:targetName/imagesets/:imageSetName"
              element={<ImageSetDetail />}
            />
            <Route path="/oc-mirror/targets/:name" element={<MirrorTargetDetail />} />
            <Route path="/oc-mirror/imagesets" element={<ImageSetList />} />
            <Route path="/oc-mirror/failed" element={<FailedImages crossTarget />} />
          </Routes>
        </main>
      </div>
    </BrowserRouter>
  );
}

const container = document.getElementById('root');
if (!container) throw new Error('Missing #root element in index.html');
createRoot(container).render(<App />);
