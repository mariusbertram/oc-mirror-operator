import React from 'react';
import ReactDOM from 'react-dom';
import { BrowserRouter, Redirect, Route, Switch, useLocation } from 'react-router-dom';
import {
  Brand,
  Masthead,
  MastheadContent,
  MastheadMain,
  Nav,
  NavItem,
  NavList,
  Page,
  PageSidebar,
  PageSidebarBody,
  Title,
} from '@patternfly/react-core';
import '@patternfly/react-core/dist/styles/base.css';
import { MirrorTargetList } from '../pages/MirrorTargets/MirrorTargetList';
import { MirrorTargetDetail } from '../pages/MirrorTargets/MirrorTargetDetail';
import { FailedImages } from '../pages/MirrorTargets/FailedImages';
import { CatalogBrowser } from '../pages/CatalogBrowser/CatalogBrowser';
import { ImageSetList } from '../pages/ImageSets/ImageSetList';
import { ImageSetDetail } from '../pages/ImageSets/ImageSetDetail';

const NavWithHighlight: React.FC = () => {
  const location = useLocation();
  const activeFor = (prefix: string) => location.pathname.startsWith(prefix);

  return (
    <Nav aria-label="OC Mirror navigation">
      <NavList>
        <NavItem itemId="targets" to="/oc-mirror/targets" isActive={activeFor('/oc-mirror/targets')}>
          Mirror Targets
        </NavItem>
        <NavItem itemId="imagesets" to="/oc-mirror/imagesets" isActive={location.pathname === '/oc-mirror/imagesets'}>
          ImageSets
        </NavItem>
        <NavItem itemId="failed" to="/oc-mirror/failed" isActive={location.pathname === '/oc-mirror/failed'}>
          Failed Images
        </NavItem>
      </NavList>
    </Nav>
  );
};

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
        <NavWithHighlight />
      </PageSidebarBody>
    </PageSidebar>
  );

  return (
    <Page header={header} sidebar={sidebar}>
      <Switch>
        <Route exact path="/">
          <Redirect to="/oc-mirror/targets" />
        </Route>
        <Route exact path="/targets">
          <Redirect to="/oc-mirror/targets" />
        </Route>
        {/* ImageSets */}
        <Route exact path="/oc-mirror/imagesets" component={ImageSetList} />
        <Route exact path="/oc-mirror/targets/:targetName/imagesets/:imageSetName" component={ImageSetDetail} />
        {/* Catalog browser */}
        <Route
          path="/oc-mirror/targets/:targetName/namespaces/:namespace/imagesets/:imageSetName/catalogs/:slug"
          component={CatalogBrowser}
        />
        {/* Failed images: per-target and cross-target */}
        <Route exact path="/oc-mirror/failed" render={() => <FailedImages crossTarget />} />
        <Route exact path="/oc-mirror/targets/:name/failures" component={FailedImages} />
        {/* MirrorTarget detail and list (last — catches /oc-mirror/targets/:name) */}
        <Route exact path="/oc-mirror/targets" component={MirrorTargetList} />
        <Route path="/oc-mirror/targets/:name" component={MirrorTargetDetail} />
      </Switch>
    </Page>
  );
};

const container = document.getElementById('root')!;
ReactDOM.render(
  <React.StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </React.StrictMode>,
  container,
);
