import React, { useEffect } from 'react';
import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { setFetchImpl } from '../../api/client';
import { CatalogBrowser } from '../../pages/CatalogBrowser/CatalogBrowser';

const CatalogBrowserPage: React.FC = () => {
  useEffect(() => {
    setFetchImpl((url, init) => consoleFetch(url, init));
  }, []);

  return <CatalogBrowser />;
};

export default CatalogBrowserPage;
