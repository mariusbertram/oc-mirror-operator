import React, { useEffect, useState } from 'react';
import {
  Alert,
  Badge,
  Button,
  Card,
  CardBody,
  PageSection,
  Spinner,
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
  SearchInput,
} from '@patternfly/react-core';
import {
  Table,
  Thead,
  Tr,
  Th,
  Tbody,
  Td,
} from '@patternfly/react-table';
import { Link, useParams, RouteComponentProps } from 'react-router-dom';
import { getImageFailures } from '../../api/client';
import type { FailedImageDetail, ImageFailuresResponse } from '../../api/types';

export const FailedImages: React.FC<Partial<RouteComponentProps<{ name: string }>>> = (props) => {
  const { match } = props;
  const params = useParams<{ name: string }>();
  const name = match?.params?.name || params.name || window.location.pathname.match(/\/oc-mirror\/targets\/([^/]+)/)?.[1];
  const [failures, setFailures] = useState<ImageFailuresResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState('');

  const load = () => {
    if (!name) return;
    setLoading(true);
    getImageFailures(name)
      .then(setFailures)
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    load();
  }, [name]);

  if (loading && !failures) return <PageSection><Spinner /></PageSection>;

  if (error) {
    return (
      <PageSection>
        <Alert variant="danger" title="Failed to load image failures" isInline>{error}</Alert>
      </PageSection>
    );
  }

  const all = [
    ...(failures?.failed || []).map(f => ({ ...f, isPermanent: true })),
    ...(failures?.pending || []).map(f => ({ ...f, isPermanent: false })),
  ];

  const filtered = all.filter(f => 
    !search || 
    f.destination.toLowerCase().includes(search.toLowerCase()) ||
    f.source.toLowerCase().includes(search.toLowerCase()) ||
    f.lastError?.toLowerCase().includes(search.toLowerCase())
  );

  return (
    <PageSection>
      <Link to={`/oc-mirror/targets/${name}`}>← Back to {name}</Link>
      <Title headingLevel="h1" style={{ margin: '1rem 0' }}>Image Failures & Pending: {name}</Title>

      <Toolbar style={{ marginBottom: '1rem' }}>
        <ToolbarContent>
          <ToolbarItem>
            <SearchInput
              placeholder="Filter images..."
              value={search}
              onChange={(_e, v) => setSearch(v)}
              onClear={() => setSearch('')}
            />
          </ToolbarItem>
          <ToolbarItem>
            <Button variant="secondary" onClick={load} isDisabled={loading}>
              Refresh
            </Button>
          </ToolbarItem>
          <ToolbarItem align={{ default: 'alignRight' }}>
            <Badge isRead>{failures?.failed.length || 0} Failed</Badge>
            {' '}
            <Badge isRead style={{ backgroundColor: 'var(--pf-v5-global--warning-color--100)' }}>
              {failures?.pending.length || 0} Pending
            </Badge>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>

      <Table aria-label="Image Failures">
        <Thead>
          <Tr>
            <Th>Status</Th>
            <Th>Image</Th>
            <Th>ImageSet</Th>
            <Th>Last Error</Th>
            <Th>Retries</Th>
          </Tr>
        </Thead>
        <Tbody>
          {filtered.length === 0 ? (
            <Tr><Td colSpan={5}>No images matching filter</Td></Tr>
          ) : (
            filtered.map((f, i) => (
              <Tr key={i}>
                <Td>
                  {f.isPermanent ? (
                    <Label color="red">Failed</Label>
                  ) : (
                    <Label color="orange">Pending</Label>
                  )}
                </Td>
                <Td>
                  <div style={{ fontSize: '0.875rem', fontWeight: 'bold' }}>{f.destination}</div>
                  <div style={{ fontSize: '0.75rem', color: 'var(--pf-v5-global--Color--300)' }}>from {f.source}</div>
                </Td>
                <Td><Badge isRead>{f.imageSet}</Badge></Td>
                <Td style={{ maxWidth: '400px', wordBreak: 'break-word', fontSize: '0.875rem' }}>
                  {f.lastError || '-'}
                </Td>
                <Td>{f.retryCount || 0}</Td>
              </Tr>
            ))
          )}
        </Tbody>
      </Table>
    </PageSection>
  );
};

// Helper for Label if not imported correctly or if using older PF
const Label: React.FC<{ color: 'red' | 'orange' | 'green', children: React.ReactNode }> = ({ color, children }) => {
  const colors = {
    red: 'var(--pf-v5-global--danger-color--100)',
    orange: 'var(--pf-v5-global--warning-color--100)',
    green: 'var(--pf-v5-global--success-color--100)',
  };
  return (
    <span style={{ 
      backgroundColor: colors[color], 
      color: '#fff', 
      padding: '0.125rem 0.5rem', 
      borderRadius: '1rem',
      fontSize: '0.75rem',
      fontWeight: 'bold',
      whiteSpace: 'nowrap'
    }}>
      {children}
    </span>
  );
};
