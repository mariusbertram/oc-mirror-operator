import React from 'react';
import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { setFetchImpl, setApiBaseUrl } from '../../api/client';
import { ImageSetDetail } from '../../pages/ImageSets/ImageSetDetail';

setFetchImpl((url, init) => consoleFetch(url, init));
setApiBaseUrl('/api/proxy/plugin/oc-mirror-operator/resourceapi');

const ImageSetDetailPage: React.FC<any> = (props) => <ImageSetDetail {...props} />;

export default ImageSetDetailPage;
