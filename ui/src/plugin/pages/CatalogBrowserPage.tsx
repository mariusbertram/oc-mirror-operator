import React from 'react';
import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { setFetchImpl, setApiBaseUrl } from '../../api/client';
import { CatalogBrowser } from '../../pages/CatalogBrowser/CatalogBrowser';

setFetchImpl((url, init) => consoleFetch(url, init));
setApiBaseUrl('/api/proxy/plugin/oc-mirror-operator/resourceapi');

const CatalogBrowserPage: React.FC<any> = (props) => <CatalogBrowser {...props} />;

export default CatalogBrowserPage;
