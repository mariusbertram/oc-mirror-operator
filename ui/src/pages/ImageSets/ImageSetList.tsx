import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  Button,
  Content,
  EmptyState,
  EmptyStateBody,
  EmptyStateVariant,
  Flex,
  FlexItem,
  FormSelect,
  FormSelectOption,
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
import { Link } from 'react-router-dom';
import { listTargets, getTarget } from '../../api/client';
import type { ImageSetSummary, TargetSummary } from '../../api/types';
import { StatusPill, computeStatus } from '../../components/StatusPill';
import { ProgressBar } from '../../components/ProgressBar';
import '../../components/plugin-styles.css';

interface ImageSetRow extends ImageSetSummary {
  targetName: string;
  targetNamespace: string;
}

export const ImageSetList: React.FC = () => {
  const [rows, setRows] = useState<ImageSetRow[]>([]);
  const [targets, setTargets] = useState<TargetSummary[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState('');
  const [filterTarget, setFilterTarget] = useState('All');

  const load = async () => {
    setLoading(true);
    setError(null);
    try {
      const ts = await listTargets();
      setTargets(ts);
      const details = await Promise.all(ts.map((t) => getTarget(t.name)));
      const allRows: ImageSetRow[] = details.flatMap((d) =>
        d.imageSets.map((is) => ({
          ...is,
          targetName: d.name,
          targetNamespace: d.namespace,
        })),
      );
      setRows(allRows);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    load();
    const interval = setInterval(load, 30_000);
    return () => clearInterval(interval);
  }, []);

  const filtered = useMemo(
    () =>
      rows.filter(
        (is) =>
          (filterTarget === 'All' || is.targetName === filterTarget) &&
          (!search || is.name.toLowerCase().includes(search.toLowerCase())),
      ),
    [rows, search, filterTarget],
  );

  if (loading && rows.length === 0) {
    return <PageSection><Spinner /></PageSection>;
  }

  if (error) {
    return (
      <PageSection>
        <Alert variant="danger" title="Failed to load ImageSets" isInline>{error}</Alert>
      </PageSection>
    );
  }

  return (
    <PageSection>
      <Flex alignItems={{ default: 'alignItemsFlexStart' }} style={{ marginBottom: 'var(--pf-v6-global--spacer--md)' }}>
        <FlexItem grow={{ default: 'grow' }}>
          <Content>
            <Content component="h1">ImageSets</Content>
            <Content component="p">
              An ImageSet declares a slice of releases, operator catalogs, and additional images to mirror.
            </Content>
          </Content>
        </FlexItem>
        <FlexItem>
          <Button variant="secondary" onClick={load} isDisabled={loading}>Refresh</Button>
        </FlexItem>
      </Flex>

      <Toolbar>
        <ToolbarContent>
          <ToolbarItem>
            <SearchInput
              placeholder="Filter by name…"
              value={search}
              onChange={(_e, v) => setSearch(v)}
              onClear={() => setSearch('')}
            />
          </ToolbarItem>
          <ToolbarItem>
            <FormSelect
              value={filterTarget}
              onChange={(_e, v) => setFilterTarget(v)}
              aria-label="Filter by MirrorTarget"
              style={{ minWidth: 220 }}
            >
              <FormSelectOption value="All" label="All targets" />
              {targets.map((t) => (
                <FormSelectOption key={t.name} value={t.name} label={t.name} />
              ))}
            </FormSelect>
          </ToolbarItem>
          <ToolbarItem align={{ default: 'alignEnd' }} variant="pagination">
            <span className="mirror-toolbar-count">
              {filtered.length} of {rows.length}
            </span>
          </ToolbarItem>
        </ToolbarContent>
      </Toolbar>

      {filtered.length === 0 ? (
        <EmptyState variant={EmptyStateVariant.lg}>
          <Title headingLevel="h2">
            {rows.length === 0 ? 'No ImageSets found' : 'No results match filter'}
          </Title>
          <EmptyStateBody>
            {rows.length === 0
              ? 'Create an ImageSet and assign it to a MirrorTarget to start mirroring.'
              : 'Clear the filter to see all ImageSets.'}
          </EmptyStateBody>
        </EmptyState>
      ) : (
        <Table aria-label="ImageSets" variant="compact">
          <Thead>
            <Tr>
              <Th>Name</Th>
              <Th>MirrorTarget</Th>
              <Th>Status</Th>
              <Th style={{ minWidth: 220 }}>Progress</Th>
              <Th>Resources</Th>
            </Tr>
          </Thead>
          <Tbody>
            {filtered.map((is) => {
              const status = computeStatus(is.total, is.mirrored, is.pending, is.failed);
              return (
                <Tr key={`${is.targetName}/${is.name}`}>
                  <Td dataLabel="Name">
                    <Link to={`/oc-mirror/targets/${is.targetName}/imagesets/${is.name}`} className="mirror-link-strong">
                      {is.name}
                    </Link>
                  </Td>
                  <Td dataLabel="MirrorTarget">
                    <Link to={`/oc-mirror/targets/${is.targetName}`}>
                      <Label isCompact color="grey">{is.targetName}</Label>
                    </Link>
                  </Td>
                  <Td dataLabel="Status">
                    <StatusPill status={status} />
                  </Td>
                  <Td dataLabel="Progress">
                    <ProgressBar total={is.total} mirrored={is.mirrored} pending={is.pending} failed={is.failed} />
                  </Td>
                  <Td dataLabel="Resources" className="mirror-toolbar-count">
                    {is.resources.length}
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
