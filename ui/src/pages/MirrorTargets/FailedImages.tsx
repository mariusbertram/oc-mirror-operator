import React, { useEffect, useState } from 'react';
import {
  Alert,
  Button,
  PageSection,
  SearchInput,
  Spinner,
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from '@patternfly/react-core';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { useParams } from 'react-router';
import { Link } from 'react-router-dom';
import { getImageFailures, listTargets } from '../../api/client';
import type { FailedImageDetail } from '../../api/types';
import '../../components/plugin-styles.css';

interface FailedImageRow extends FailedImageDetail {
  targetName?: string;
  isPermanent: boolean;
}

interface FailedImagesProps {
  crossTarget?: boolean;
}

export const FailedImages: React.FC<FailedImagesProps> = ({ crossTarget }) => {
  const params = useParams<{ name: string }>();
  const name =
    params.name ||
    (!crossTarget ? window.location.pathname.match(/\/oc-mirror\/targets\/([^/]+)/)?.[1] : undefined);

  const [rows, setRows] = useState<FailedImageRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState('');

  const load = async () => {
    setLoading(true);
    setError(null);
    try {
      if (crossTarget || !name) {
        // Fetch failures for all targets
        const targets = await listTargets();
        const results = await Promise.allSettled(
          targets.map((t) => getImageFailures(t.name).then((r) => ({ target: t.name, r }))),
        );
        const all: FailedImageRow[] = [];
        for (const res of results) {
          if (res.status === 'fulfilled') {
            const { target: tName, r } = res.value;
            all.push(...(r.failed || []).map((f) => ({ ...f, targetName: tName, isPermanent: true })));
            all.push(...(r.pending || []).map((f) => ({ ...f, targetName: tName, isPermanent: false })));
          }
        }
        setRows(all);
      } else {
        const r = await getImageFailures(name);
        setRows([
          ...(r.failed || []).map((f) => ({ ...f, isPermanent: true })),
          ...(r.pending || []).map((f) => ({ ...f, isPermanent: false })),
        ]);
      }
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    load();
  }, [name, crossTarget]);

  if (loading && rows.length === 0) {
    return <PageSection><Spinner /></PageSection>;
  }

  if (error) {
    return (
      <PageSection>
        <Alert variant="danger" title="Failed to load image failures" isInline>{error}</Alert>
      </PageSection>
    );
  }

  const filtered = rows.filter(
    (f) =>
      !search ||
      f.destination.toLowerCase().includes(search.toLowerCase()) ||
      f.source.toLowerCase().includes(search.toLowerCase()) ||
      f.imageSet.toLowerCase().includes(search.toLowerCase()) ||
      (f.lastError || '').toLowerCase().includes(search.toLowerCase()),
  );

  const permanentCount = filtered.filter((f) => f.isPermanent).length;
  const pendingCount = filtered.filter((f) => !f.isPermanent).length;

  return (
    <PageSection>
      {!crossTarget && name && (
        <div style={{ marginBottom: 8 }}>
          <Link to={`/oc-mirror/targets/${name}`} style={{ fontSize: 13 }}>← Back to {name}</Link>
        </div>
      )}

      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
        <div>
          <Title headingLevel="h1">
            {crossTarget ? 'Failed Images' : `Image Failures — ${name}`}
          </Title>
          <p style={{ margin: '4px 0 0', color: 'var(--pf-v6-global--Color--200)' }}>
            Images that exhausted all retries. Fix the upstream error or spec, then trigger a re-poll.
          </p>
        </div>
        <Button variant="secondary" onClick={load} isDisabled={loading}>
          Retry all
        </Button>
      </div>

      <Toolbar style={{ marginBottom: 0, paddingLeft: 0 }}>
        <ToolbarContent>
          <ToolbarItem>
            <SearchInput
              placeholder="Filter by image, error, or ImageSet…"
              value={search}
              onChange={(_e, v) => setSearch(v)}
              onClear={() => setSearch('')}
            />
          </ToolbarItem>
          <ToolbarItem align={{ default: 'alignEnd' }}>
            <span style={{ fontSize: 13, color: 'var(--pf-v6-global--Color--200)' }}>
              {filtered.length} items
              {permanentCount > 0 && (
                <span style={{ marginLeft: 8, color: 'var(--pf-v6-global--danger-color--100)' }}>
                  {permanentCount} failed
                </span>
              )}
              {pendingCount > 0 && (
                <span style={{ marginLeft: 8, color: 'var(--pf-v6-global--warning-color--100)' }}>
                  {pendingCount} pending
                </span>
              )}
            </span>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>

      {filtered.length === 0 ? (
        <div style={{ padding: '48px 0', textAlign: 'center', color: 'var(--pf-v6-global--Color--200)' }}>
          {rows.length === 0 ? 'No failed or pending images.' : 'No items match the filter.'}
        </div>
      ) : (
        <Table aria-label="Failed Images" variant="compact">
          <Thead>
            <Tr>
              <Th>Status</Th>
              <Th>Source image</Th>
              <Th>ImageSet</Th>
              {crossTarget && <Th>Target</Th>}
              <Th>Error</Th>
              <Th>Retries</Th>
            </Tr>
          </Thead>
          <Tbody>
            {filtered.map((f, i) => (
              <Tr key={i}>
                <Td>
                  <StatusBadge permanent={f.isPermanent} />
                </Td>
                <Td>
                  <code className="mirror-mono" style={{ fontSize: 11, wordBreak: 'break-all' }}>
                    {f.destination}
                  </code>
                  <div style={{ fontSize: 11, color: 'var(--pf-v6-global--Color--200)', marginTop: 2 }}>
                    from {f.source}
                  </div>
                </Td>
                <Td>
                  <span className="mirror-tag">{f.imageSet}</span>
                </Td>
                {crossTarget && (
                  <Td>
                    {f.targetName ? (
                      <Link to={`/oc-mirror/targets/${f.targetName}`}>{f.targetName}</Link>
                    ) : '—'}
                  </Td>
                )}
                <Td style={{ maxWidth: 360, wordBreak: 'break-word', fontSize: '0.875rem', color: 'var(--pf-v6-global--danger-color--100)' }}>
                  {f.lastError || '—'}
                </Td>
                <Td style={{ fontVariantNumeric: 'tabular-nums' }}>{f.retryCount ?? 0}</Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      )}
    </PageSection>
  );
};

const StatusBadge: React.FC<{ permanent: boolean }> = ({ permanent }) => (
  <span style={{
    display: 'inline-block',
    padding: '1px 8px',
    borderRadius: 12,
    fontSize: 12,
    fontWeight: 600,
    background: permanent ? 'var(--pf-v6-global--danger-color--100)' : 'var(--pf-v6-global--warning-color--100)',
    color: '#fff',
    whiteSpace: 'nowrap',
  }}>
    {permanent ? 'Failed' : 'Pending'}
  </span>
);
