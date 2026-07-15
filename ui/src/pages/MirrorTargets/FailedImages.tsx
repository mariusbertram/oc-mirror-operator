import React, { useEffect, useState } from 'react';
import {
  Alert,
  Button,
  Content,
  Flex,
  FlexItem,
  Label,
  PageSection,
  SearchInput,
  Spinner,
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from '@patternfly/react-core';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { Link, useParams } from 'react-router';
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
        <div style={{ marginBottom: 'var(--pf-v6-global--spacer--sm)' }}>
          <Link to={`/oc-mirror/targets/${name}`}>← Back to {name}</Link>
        </div>
      )}

      <Flex
        alignItems={{ default: 'alignItemsFlexStart' }}
        style={{ marginBottom: 'var(--pf-v6-global--spacer--md)' }}
      >
        <FlexItem grow={{ default: 'grow' }}>
          <Content>
            <Title headingLevel="h1">
              {crossTarget ? 'Failed Images' : `Image Failures — ${name}`}
            </Title>
            <Content component="p">
              Images that exhausted all retries. Fix the upstream error or spec, then trigger a re-poll.
            </Content>
          </Content>
        </FlexItem>
        <FlexItem>
          <Button variant="secondary" onClick={load} isDisabled={loading}>
            Refresh
          </Button>
        </FlexItem>
      </Flex>

      <Toolbar>
        <ToolbarContent>
          <ToolbarItem>
            <SearchInput
              placeholder="Filter by image, error, or ImageSet…"
              value={search}
              onChange={(_e, v) => setSearch(v)}
              onClear={() => setSearch('')}
            />
          </ToolbarItem>
          <ToolbarItem align={{ default: 'alignEnd' }} variant="pagination">
            <Flex gap={{ default: 'gapSm' }} alignItems={{ default: 'alignItemsCenter' }}>
              <FlexItem>
                <span className="mirror-toolbar-count">{filtered.length} items</span>
              </FlexItem>
              {permanentCount > 0 && (
                <FlexItem>
                  <Label isCompact color="red">{permanentCount} failed</Label>
                </FlexItem>
              )}
              {pendingCount > 0 && (
                <FlexItem>
                  <Label isCompact color="orange">{pendingCount} pending</Label>
                </FlexItem>
              )}
            </Flex>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>

      {filtered.length === 0 ? (
        <div className="mirror-empty-center">
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
                <Td dataLabel="Status">
                  <Label isCompact color={f.isPermanent ? 'red' : 'orange'}>
                    {f.isPermanent ? 'Failed' : 'Pending'}
                  </Label>
                </Td>
                <Td dataLabel="Source image">
                  <code className="mirror-mono" style={{ fontSize: 11, wordBreak: 'break-all' }}>
                    {f.destination}
                  </code>
                  <div className="mirror-sub-text">from {f.source}</div>
                </Td>
                <Td dataLabel="ImageSet">
                  <Label isCompact color="grey">{f.imageSet}</Label>
                </Td>
                {crossTarget && (
                  <Td dataLabel="Target">
                    {f.targetName ? (
                      <Link to={`/oc-mirror/targets/${f.targetName}`}>{f.targetName}</Link>
                    ) : '—'}
                  </Td>
                )}
                <Td dataLabel="Error" className="mirror-error-cell">
                  {f.lastError || '—'}
                </Td>
                <Td dataLabel="Retries" className="mirror-toolbar-count">{f.retryCount ?? 0}</Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      )}
    </PageSection>
  );
};
