import React from 'react';
import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { setFetchImpl, setApiBaseUrl } from '../../api/client';
import { ImageSetList } from '../../pages/ImageSets/ImageSetList';

setFetchImpl((url, init) => consoleFetch(url, init));
setApiBaseUrl('/api/proxy/plugin/oc-mirror-operator/resourceapi');

const ImageSetListPage: React.FC<any> = (props) => <ImageSetList {...props} />;

export default ImageSetListPage;
