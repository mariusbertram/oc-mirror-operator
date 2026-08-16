import React from 'react';
import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { setFetchImpl, setApiBaseUrl } from '../../api/client';
import { FailedImages } from '../../pages/MirrorTargets/FailedImages';

setFetchImpl((url, init) => consoleFetch(url, init));
setApiBaseUrl('/api/proxy/plugin/oc-mirror-operator/resourceapi');

const FailedImagesPage: React.FC<Record<string, unknown>> = (props) => <FailedImages {...props} />;

export default FailedImagesPage;
