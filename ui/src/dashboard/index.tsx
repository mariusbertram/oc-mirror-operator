import React from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';
import { Page, PageSidebar, PageSidebarBody, Nav, NavItem, NavList, Masthead, MastheadMain, MastheadContent, Title } from '@patternfly/react-core';
import '@patternfly/react-core/dist/styles/base.css';
import { MirrorTargetList } from '../pages/MirrorTargets/MirrorTargetList';
import { MirrorTargetDetail } from '../pages/MirrorTargets/MirrorTargetDetail';
import { CatalogBrowser } from '../pages/CatalogBrowser/CatalogBrowser';

const App: React.FC = () => {
  const [sidebarOpen, setSidebarOpen] = React.useState(true);

  const header = (
    <Masthead>
      <MastheadMain>
        <Title headingLevel="h1" style={{ color: '#fff', margin: 0 }}>
          OC Mirror Operator
        </Title>
      </MastheadMain>
      <MastheadContent />
    </Masthead>
  );

  const sidebar = (
    <PageSidebar isSidebarOpen={sidebarOpen}>
      <PageSidebarBody>
        <Nav>
          <NavList>
            <NavItem itemId="targets" to="/targets">
              Mirror Targets
            </NavItem>
          </NavList>
        </Nav>
      </PageSidebarBody>
    </PageSidebar>
  );

  return (
    <Page header={header} sidebar={sidebar}>
      <Routes>
        <Route path="/" element={<Navigate to="/targets" replace />} />
        <Route path="/targets" element={<MirrorTargetList />} />
        <Route path="/targets/:name" element={<MirrorTargetDetail />} />
        <Route
          path="/targets/:targetName/catalogs/:slug"
          element={<CatalogBrowser />}
        />
      </Routes>
    </Page>
  );
};

const container = document.getElementById('root')!;
const root = createRoot(container);
root.render(
  <React.StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </React.StrictMode>,
);
