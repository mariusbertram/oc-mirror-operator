import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  Button,
  EmptyState,
  EmptyStateBody,
  EmptyStateVariant,
  PageSection,
  SearchInput,
  Spinner,
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from '@patternfly/react-core';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { Link } from 'react-router';
import { listTargets } from '../../api/client';
import type { TargetSummary } from '../../api/types';
import { StatusPill, computeStatus } from '../../components/StatusPill';
import { ProgressBar } from '../../components/ProgressBar';
import '../../components/plugin-styles.css';

export const MirrorTargetList: React.FC = () => {
  const [targets, setTargets] = useState<TargetSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState('');

  const load = () => {
    setLoading(true);
    listTargets()
      .then(setTargets)
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false));
  };

  useEffect(() => {
    load();
    const interval = setInterval(load, 30_000);
    return () => clearInterval(interval);
  }, []);

  const filtered = useMemo(
    () =>
      targets.filter(
        (t) =>
          !search ||
          t.name.toLowerCase().includes(search.toLowerCase()) ||
          t.registry.toLowerCase().includes(search.toLowerCase()),
      ),
    [targets, search],
  );

  if (loading && targets.length === 0) {
    return (
      <PageSection>
        <Spinner />
      </PageSection>
    );
  }

  if (error) {
    return (
      <PageSection>
        <Alert variant="danger" title="Failed to load MirrorTargets" isInline>
          {error}
        </Alert>
      </PageSection>
    );
  }

  return (
    <PageSection>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '1rem' }}>
        <div>
          <Title headingLevel="h1">MirrorTargets</Title>
          <p style={{ margin: '4px 0 0', color: 'var(--pf-v6-global--Color--200)' }}>
            Each MirrorTarget defines a destination registry and the set of ImageSets to mirror into it.
          </p>
        </div>
      </div>

      <Toolbar style={{ marginBottom: 0, paddingLeft: 0 }}>
        <ToolbarContent>
          <ToolbarItem>
            <SearchInput
              placeholder="Filter by name or registry…"
              value={search}
              onChange={(_e, v) => setSearch(v)}
              onClear={() => setSearch('')}
            />
          </ToolbarItem>
          <ToolbarItem>
            <Button variant="secondary" onClick={load} isDisabled={loading}>
              {loading ? <Spinner size="sm" /> : 'Refresh'}
            </Button>
          </ToolbarItem>
          <ToolbarItem align={{ default: 'alignEnd' }}>
            <span style={{ fontSize: 13, color: 'var(--pf-v6-global--Color--200)' }}>
              {filtered.length} of {targets.length}
            </span>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>

      {filtered.length === 0 ? (
        <EmptyState variant={EmptyStateVariant.lg}>
          <Title headingLevel="h2">{targets.length === 0 ? 'No MirrorTargets found' : 'No results match filter'}</Title>
          <EmptyStateBody>
            {targets.length === 0
              ? 'Create a MirrorTarget to declare a destination registry and start mirroring.'
              : 'Clear the filter to see all MirrorTargets.'}
          </EmptyStateBody>
        </EmptyState>
      ) : (
        <Table aria-label="MirrorTargets" variant="compact">
          <Thead>
            <Tr>
              <Th>Name</Th>
              <Th>Registry</Th>
              <Th>Status</Th>
              <Th style={{ minWidth: 200 }}>Progress</Th>
              <Th>Total</Th>
              <Th>Mirrored</Th>
              <Th>Pending</Th>
              <Th>Failed</Th>
            </Tr>
          </Thead>
          <Tbody>
            {filtered.map((t) => {
              const status = computeStatus(t.totalImages, t.mirroredImages, t.pendingImages, t.failedImages);
              return (
                <Tr key={t.name}>
                  <Td>
                    <Link to={`/oc-mirror/targets/${t.name}`} style={{ fontWeight: 500 }}>{t.name}</Link>
                    <div style={{ fontSize: 12, color: 'var(--pf-v6-global--Color--200)' }}>{t.namespace}</div>
                  </Td>
                  <Td>
                    <code className="mirror-mono">{t.registry}</code>
                  </Td>
                  <Td>
                    <StatusPill status={status} />
                  </Td>
                  <Td>
                    <ProgressBar
                      total={t.totalImages}
                      mirrored={t.mirroredImages}
                      pending={t.pendingImages}
                      failed={t.failedImages}
                    />
                  </Td>
                  <Td style={{ fontVariantNumeric: 'tabular-nums' }}>{t.totalImages.toLocaleString()}</Td>
                  <Td style={{ fontVariantNumeric: 'tabular-nums', color: 'var(--pf-v6-global--success-color--100)' }}>
                    {t.mirroredImages.toLocaleString()}
                  </Td>
                  <Td style={{ fontVariantNumeric: 'tabular-nums', color: t.pendingImages > 0 ? 'var(--pf-v6-global--warning-color--100)' : undefined }}>
                    {t.pendingImages.toLocaleString()}
                  </Td>
                  <Td style={{ fontVariantNumeric: 'tabular-nums', color: t.failedImages > 0 ? 'var(--pf-v6-global--danger-color--100)' : undefined }}>
                    {t.failedImages.toLocaleString()}
                  </Td>
                </Tr>
              );
            })}
          </Tbody>
        </Table>
      )}
    </PageSection>
  );
};
