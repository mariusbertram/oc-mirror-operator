import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  Button,
  EmptyState,
  EmptyStateBody,
  EmptyStateVariant,
  SearchInput,
  Spinner,
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from '@patternfly/react-core';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { Link } from '@router';
import { listTargets } from '../../api/client';
import type { TargetSummary } from '../../api/types';
import { StatusPill, computeStatus } from '../../components/StatusPill';
import { ProgressBar } from '../../components/ProgressBar';
import '../../components/plugin-styles.css';
import { PageSection } from '../../components/ThemePageSection';

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
        <Alert variant="danger" title="Failed to load Mirror Targets" isInline>
          {error}
        </Alert>
      </PageSection>
    );
  }

  return (
    <PageSection>
      <div style={{ marginBottom: 'var(--pf-v6-global--spacer--md)' }}>
        <Title headingLevel="h1">Mirror Targets</Title>
        <p>
          Each Mirror Target defines a destination registry and the set of ImageSets to mirror into it.
        </p>
      </div>

      <Toolbar>
        <ToolbarContent className="mirror-filter-toolbar">
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
          <ToolbarItem className="mirror-toolbar-pagination" variant="pagination">
            <span className="mirror-toolbar-count">
              {filtered.length} of {targets.length}
            </span>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>

      {filtered.length === 0 ? (
        <EmptyState variant={EmptyStateVariant.lg}>
          <Title headingLevel="h2">
            {targets.length === 0 ? 'No Mirror Targets found' : 'No results match filter'}
          </Title>
          <EmptyStateBody>
            {targets.length === 0
              ? 'Create a Mirror Target to declare a destination registry and start mirroring.'
              : 'Clear the filter to see all Mirror Targets.'}
          </EmptyStateBody>
        </EmptyState>
      ) : (
        <Table aria-label="Mirror Targets" variant="compact">
          <Thead>
            <Tr>
              <Th>Name</Th>
              <Th>Registry</Th>
              <Th>Status</Th>
              <Th style={{ minWidth: 220 }}>Progress</Th>
            </Tr>
          </Thead>
          <Tbody>
            {filtered.map((t) => {
              const status = computeStatus(t.totalImages, t.mirroredImages, t.pendingImages, t.failedImages);
              return (
                <Tr key={t.name}>
                  <Td dataLabel="Name">
                    <Link to={`/oc-mirror/targets/${t.name}`} className="mirror-link-strong">{t.name}</Link>
                    <div className="mirror-sub-text">{t.namespace}</div>
                  </Td>
                  <Td dataLabel="Registry">
                    <code className="mirror-mono">{t.registry}</code>
                  </Td>
                  <Td dataLabel="Status">
                    <StatusPill status={status} />
                  </Td>
                  <Td dataLabel="Progress">
                    <ProgressBar
                      total={t.totalImages}
                      mirrored={t.mirroredImages}
                      pending={t.pendingImages}
                      failed={t.failedImages}
                    />
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
