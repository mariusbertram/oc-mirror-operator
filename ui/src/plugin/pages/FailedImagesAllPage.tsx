import React from 'react';
import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { setFetchImpl, setApiBaseUrl } from '../../api/client';
import { FailedImages } from '../../pages/MirrorTargets/FailedImages';

setFetchImpl((url, init) => consoleFetch(url, init));
setApiBaseUrl('/api/proxy/plugin/oc-mirror-operator/resourceapi');

const FailedImagesAllPage: React.FC<any> = (props) => <FailedImages {...props} crossTarget />;

export default FailedImagesAllPage;
