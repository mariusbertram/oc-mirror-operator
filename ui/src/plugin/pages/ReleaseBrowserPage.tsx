import React from 'react';
import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { setFetchImpl, setApiBaseUrl } from '../../api/client';
import { ReleaseBrowser } from '../../pages/ReleaseBrowser/ReleaseBrowser';

setFetchImpl((url, init) => consoleFetch(url, init));
setApiBaseUrl('/api/proxy/plugin/oc-mirror-operator/resourceapi');

const ReleaseBrowserPage: React.FC<any> = (props) => <ReleaseBrowser {...props} />;

export default ReleaseBrowserPage;
