import React, { useEffect, useState } from 'react';
import {
  Alert,
  Button,
  Checkbox,
  PageSection,
  SearchInput,
  Spinner,
  TextInput,
  Title,
  Toolbar,
  ToolbarContent,
  ToolbarItem,
} from '@patternfly/react-core';
import { useParams } from 'react-router';
import { Link } from 'react-router-dom';
import { getOcpChannels, getReleases, patchReleases } from '../../api/client';
import type { OcpChannelEntry, ReleaseChannel, ReleaseSpec } from '../../api/types';
import '../../components/plugin-styles.css';

type ReleaseBrowserParams = 'targetName' | 'namespace' | 'imageSetName';

// Built-in fallback for when the /api/v1/releases/channels endpoint is unreachable.
const FALLBACK_CHANNELS: OcpChannelEntry[] = (() => {
  const types = ['stable', 'fast', 'eus', 'candidate'];
  const versions = ['4.14', '4.15', '4.16', '4.17', '4.18', '4.19'];
  const result: OcpChannelEntry[] = [];
  for (const ver of versions) {
    const minor = parseInt(ver.split('.')[1], 10);
    for (const t of types) {
      if (t === 'eus' && minor % 2 !== 0) continue;
      result.push({ name: `${t}-${ver}`, type: 'ocp', version: ver });
    }
  }
  return result;
})();

export const ReleaseBrowser: React.FC = () => {
  const params = useParams<ReleaseBrowserParams>();
  let { targetName, namespace, imageSetName } = params;

  if (!targetName || !namespace || !imageSetName) {
    const m = window.location.pathname.match(
      /\/oc-mirror\/targets\/([^/]+)\/namespaces\/([^/]+)\/imagesets\/([^/]+)\/releases/,
    );
    if (m) {
      targetName = m[1];
      namespace = m[2];
      imageSetName = m[3];
    }
  }

  const [channels, setChannels] = useState<ReleaseChannel[]>([]);
  const [availableChannels, setAvailableChannels] = useState<OcpChannelEntry[]>(FALLBACK_CHANNELS);
  const [graph, setGraph] = useState(false);
  const [architectures, setArchitectures] = useState('');
  const [dirty, setDirty] = useState(false);
  const [loading, setLoading] = useState(true);
  const [channelsLoading, setChannelsLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [successMsg, setSuccessMsg] = useState<string | null>(null);
  const [search, setSearch] = useState('');
  const [customChannel, setCustomChannel] = useState('');

  useEffect(() => {
    getOcpChannels()
      .then((entries) => {
        if (entries.length > 0) setAvailableChannels(entries);
      })
      .catch(() => {
        /* keep FALLBACK_CHANNELS already set */
      })
      .finally(() => setChannelsLoading(false));
  }, []);

  useEffect(() => {
    if (!namespace || !imageSetName) return;
    setLoading(true);
    getReleases(namespace, imageSetName)
      .then((spec: ReleaseSpec) => {
        setGraph(spec.graph ?? false);
        setArchitectures((spec.architectures ?? []).join(','));
        setChannels(spec.channels ?? []);
        setDirty(false);
      })
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false));
  }, [namespace, imageSetName]);

  const isChannelAdded = (name: string) => channels.some((c) => c.name === name);

  const addChannel = (name: string, type = 'ocp') => {
    if (isChannelAdded(name)) return;
    setChannels((prev) => [...prev, { name, type }]);
    setDirty(true);
  };

  const removeChannel = (name: string) => {
    setChannels((prev) => prev.filter((c) => c.name !== name));
    setDirty(true);
  };

  const updateChannel = (name: string, field: keyof ReleaseChannel, value: string | boolean) => {
    setChannels((prev) =>
      prev.map((c) => (c.name === name ? { ...c, [field]: value } : c)),
    );
    setDirty(true);
  };

  const handleSave = async () => {
    if (!namespace || !imageSetName) return;
    setSaving(true);
    setError(null);
    setSuccessMsg(null);
    try {
      const archList = architectures.split(',').map((s) => s.trim()).filter(Boolean);
      await patchReleases(namespace, imageSetName, {
        graph,
        architectures: archList.length > 0 ? archList : undefined,
        channels,
      });
      setDirty(false);
      setSuccessMsg('Release configuration saved.');
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  const filteredAvailable = availableChannels.filter((ch) =>
    ch.name.toLowerCase().includes(search.toLowerCase()),
  );

  if (loading) return <PageSection><Spinner /></PageSection>;

  return (
    <>
      <PageSection>
        <Toolbar>
          <ToolbarContent>
            <ToolbarItem>
              <Link to={`/oc-mirror/targets/${targetName}/imagesets/${imageSetName}`}>
                ← Back to ImageSet
              </Link>
            </ToolbarItem>
            <ToolbarItem align={{ default: 'alignEnd' }}>
              {dirty && (
                <Button variant="primary" size="sm" isLoading={saving} onClick={handleSave}>
                  Save
                </Button>
              )}
            </ToolbarItem>
          </ToolbarContent>
        </Toolbar>

        <Title headingLevel="h1" style={{ marginTop: 'var(--pf-v6-global--spacer--md)' }}>
          Release Browser — {imageSetName}
        </Title>

        {error && (
          <Alert variant="danger" title="Error" isInline style={{ marginTop: 8 }}>
            {error}
          </Alert>
        )}
        {successMsg && (
          <Alert variant="success" title={successMsg} isInline style={{ marginTop: 8 }} />
        )}

        <div style={{ display: 'flex', gap: 16, marginTop: 16, flexWrap: 'wrap', alignItems: 'center' }}>
          <Checkbox
            id="graph-checkbox"
            label="Include Cincinnati graph"
            isChecked={graph}
            onChange={(_e, checked) => { setGraph(checked); setDirty(true); }}
          />
          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <label style={{ fontSize: 13 }} htmlFor="arch-input">Architectures:</label>
            <TextInput
              id="arch-input"
              value={architectures}
              onChange={(_e, v) => { setArchitectures(v); setDirty(true); }}
              placeholder="amd64,arm64"
              style={{ width: 180, fontSize: 13 }}
            />
          </div>
        </div>
      </PageSection>

      <PageSection>
        <div className="mirror-dual-simple">
          {/* Left pane — Available OCP Channels */}
          <div className="mirror-dual-pane">
            <div className="mirror-dual-pane__header">
              <strong>Available OCP Channels</strong>
            </div>
            <div style={{ padding: '8px 12px' }}>
              <SearchInput
                placeholder="Filter channels…"
                value={search}
                onChange={(_e, v) => setSearch(v)}
                onClear={() => setSearch('')}
              />
            </div>
            <div className="mirror-dual-pane__body">
              {channelsLoading ? (
                <div style={{ padding: 16, textAlign: 'center' }}><Spinner size="md" /></div>
              ) : (
                <>
                  {filteredAvailable.map((ch) => {
                    const added = isChannelAdded(ch.name);
                    return (
                      <div key={ch.name} className="mirror-dual-row" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                        <span style={{ fontSize: 13 }}>{ch.name}</span>
                        <Button
                          variant="plain"
                          size="sm"
                          isDisabled={added}
                          onClick={() => addChannel(ch.name, ch.type)}
                          style={{ fontSize: 12, color: added ? '#6a6e73' : '#0066cc' }}
                        >
                          {added ? 'added' : '+ Add'}
                        </Button>
                      </div>
                    );
                  })}
                  {filteredAvailable.length === 0 && (
                    <div style={{ padding: 12, color: '#6a6e73', fontSize: 13 }}>No channels match.</div>
                  )}
                </>
              )}
            </div>
            <div style={{ padding: '8px 12px', borderTop: '1px solid var(--pf-v6-global--BorderColor--100)', display: 'flex', gap: 8 }}>
              <TextInput
                value={customChannel}
                onChange={(_e, v) => setCustomChannel(v)}
                placeholder="Custom channel name"
                style={{ flex: 1, fontSize: 13 }}
              />
              <Button
                variant="secondary"
                size="sm"
                isDisabled={!customChannel.trim() || isChannelAdded(customChannel.trim())}
                onClick={() => {
                  addChannel(customChannel.trim(), 'ocp');
                  setCustomChannel('');
                }}
              >
                Add
              </Button>
            </div>
          </div>

          {/* Right pane — Channels in ImageSet */}
          <div className="mirror-dual-pane">
            <div className="mirror-dual-pane__header">
              <strong>Channels in ImageSet <em>{imageSetName}</em></strong>
            </div>
            <div className="mirror-dual-pane__body">
              {channels.length === 0 && (
                <div style={{ padding: 16, color: '#6a6e73', fontSize: 13 }}>
                  No channels added yet. Use the left pane to add channels.
                </div>
              )}
              {channels.map((ch) => (
                <div
                  key={ch.name}
                  className="mirror-dual-row"
                  style={{ display: 'flex', flexDirection: 'column', gap: 4, padding: '8px 12px', position: 'relative' }}
                >
                  {/* Row 1: name + type + remove */}
                  <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                    <strong style={{ fontSize: 13, flex: 1 }}>{ch.name}</strong>
                    <select
                      value={ch.type ?? 'ocp'}
                      onChange={(e) => updateChannel(ch.name, 'type', e.target.value)}
                      style={{ fontSize: 12, padding: '1px 4px' }}
                    >
                      <option value="ocp">ocp</option>
                      <option value="okd">okd</option>
                    </select>
                    <Button
                      variant="plain"
                      size="sm"
                      onClick={() => removeChannel(ch.name)}
                      style={{ fontSize: 14, color: '#c9190b', padding: '0 4px' }}
                      aria-label={`Remove ${ch.name}`}
                    >
                      ×
                    </Button>
                  </div>
                  {/* Row 2: min/max version */}
                  <div style={{ display: 'flex', gap: 8 }}>
                    <TextInput
                      value={ch.minVersion ?? ''}
                      onChange={(_e, v) => updateChannel(ch.name, 'minVersion', v)}
                      placeholder="Min version"
                      style={{ width: 120, fontSize: 12 }}
                      aria-label="Min version"
                    />
                    <TextInput
                      value={ch.maxVersion ?? ''}
                      onChange={(_e, v) => updateChannel(ch.name, 'maxVersion', v)}
                      placeholder="Max version"
                      style={{ width: 120, fontSize: 12 }}
                      aria-label="Max version"
                    />
                  </div>
                  {/* Row 3: shortest path + full checkboxes */}
                  <div style={{ display: 'flex', gap: 16, alignItems: 'center' }}>
                    <Checkbox
                      id={`shortest-${ch.name}`}
                      label="Shortest path"
                      isChecked={ch.shortestPath ?? false}
                      onChange={(_e, checked) => updateChannel(ch.name, 'shortestPath', checked)}
                    />
                    <Checkbox
                      id={`full-${ch.name}`}
                      label="Full"
                      isChecked={ch.full ?? false}
                      onChange={(_e, checked) => updateChannel(ch.name, 'full', checked)}
                    />
                    <Checkbox
                      id={`skipsig-${ch.name}`}
                      label="Skip signature verification"
                      isChecked={ch.skipSignatureVerification ?? false}
                      onChange={(_e, checked) => updateChannel(ch.name, 'skipSignatureVerification', checked)}
                    />
                  </div>
                </div>
              ))}
            </div>
          </div>
        </div>
      </PageSection>
    </>
  );
};
