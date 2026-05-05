import React from 'react';
import ReactDOM from 'react-dom';
import { BrowserRouter, Redirect, Route, Switch } from 'react-router-dom';
import { Page, PageSidebar, PageSidebarBody, Nav, NavItem, NavList, Masthead, MastheadMain, MastheadContent, Title } from '@patternfly/react-core';
import '@patternfly/react-core/dist/styles/base.css';
import { MirrorTargetList } from '../pages/MirrorTargets/MirrorTargetList';
import { MirrorTargetDetail } from '../pages/MirrorTargets/MirrorTargetDetail';
import { FailedImages } from '../pages/MirrorTargets/FailedImages';
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
      <Switch>
        <Route exact path="/">
          <Redirect to="/oc-mirror/targets" />
        </Route>
        <Route exact path="/targets">
          <Redirect to="/oc-mirror/targets" />
        </Route>
        <Route exact path="/oc-mirror/targets" component={MirrorTargetList} />
        <Route exact path="/oc-mirror/targets/:name" component={MirrorTargetDetail} />
        <Route path="/oc-mirror/targets/:name/failures" component={FailedImages} />
        <Route
          path="/oc-mirror/targets/:targetName/namespaces/:namespace/imagesets/:imageSetName/catalogs/:slug"
          component={CatalogBrowser}
        />
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
  container
);
