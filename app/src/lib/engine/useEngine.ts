import { assertEngineModel } from './validation';
import { useCallback, useEffect, useRef, useState } from 'react';
import {
  allPages,
  connectEngine,
  disconnectEngine,
  engineCache,
  engineCall,
  engineError,
} from './client';
import type {
  EngineCall,
  EngineInfo,
  EngineRun,
  EngineRequest,
  EngineArtifact,
  Profile,
  Resource,
  PendingOperation,
} from './types';

export function useEngine(notify: (message: string) => void) {
  const [info, setInfo] = useState<EngineInfo>();
  const [connecting, setConnecting] = useState(false);
  const [syncing, setSyncing] = useState(false);
  const [error, setError] = useState('');
  const [runs, setRuns] = useState<EngineRun[]>([]);
  const [requests, setRequests] = useState<EngineRequest[]>([]);
  const [artifacts, setArtifacts] = useState<EngineArtifact[]>([]);
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [resources, setResources] = useState<Resource[]>([]);
  const [pending, setPending] = useState<PendingOperation[]>([]);
  const revision = useRef(0);
  const refreshing = useRef<number | undefined>(undefined);
  const refresh = useCallback(async () => {
    const current = revision.current;
    if (!info || refreshing.current === current) return;
    refreshing.current = current;
    setSyncing(true);
    try {
      const [runs, requests, artifacts, profiles, resources, cached] = await Promise.all([
        allPages<EngineRun>('runs'),
        allPages<EngineRequest>('requests'),
        allPages<EngineArtifact>('artifacts'),
        engineCall<{ items: Profile[] }>({ op: 'profiles' }),
        engineCall<{ items: Resource[] }>({ op: 'resources' }),
        engineCache(),
      ]);
      if (current !== revision.current) return;
      if (!Array.isArray(profiles.items) || !Array.isArray(resources.items))
        throw new Error('Engine returned an incompatible catalog.');
      for (const run of runs) assertEngineModel('Run', run);
      for (const request of requests) assertEngineModel('HumanRequest', request);
      for (const artifact of artifacts) assertEngineModel('Artifact', artifact);
      for (const profile of profiles.items) assertEngineModel('Profile', profile);
      for (const resource of resources.items) assertEngineModel('Resource', resource);
      setRuns(runs);
      setRequests(requests);
      setArtifacts(artifacts);
      setProfiles(profiles.items);
      setResources(resources.items);
      setPending(cached.pending);
      setError('');
    } catch (error) {
      if (current === revision.current) setError(engineError(error).message);
    } finally {
      if (refreshing.current === current) refreshing.current = undefined;
      if (current === revision.current) setSyncing(false);
    }
  }, [info]);
  useEffect(() => {
    void refresh();
    if (!info) return;
    const timer = setInterval(() => void refresh(), 10000);
    return () => clearInterval(timer);
  }, [refresh, info]);
  async function connect(url: string, token?: string) {
    const current = ++revision.current;
    setInfo(undefined);
    setConnecting(true);
    setError('');
    setRuns([]);
    setRequests([]);
    setArtifacts([]);
    setProfiles([]);
    setResources([]);
    setPending([]);
    try {
      const info = await connectEngine(url, token);
      assertEngineModel('Info', info);
      if (current !== revision.current) return;
      const saved = await engineCache();
      // Recover the last read model first, clearly marked until the fresh read completes.
      const pages = Object.entries(saved.cache).flatMap(([key, value]) => {
        try {
          return [{ op: JSON.parse(key).op, cursor: JSON.parse(key).cursor, value }];
        } catch {
          return [];
        }
      });
      const validPages = pages.filter((page) => {
        try {
          if (!Array.isArray((page.value as { items?: unknown[] })?.items)) return false;
          const model =
            page.op === 'runs'
              ? 'Run'
              : page.op === 'requests'
                ? 'HumanRequest'
                : page.op === 'artifacts'
                  ? 'Artifact'
                  : undefined;
          if (model)
            for (const item of (page.value as { items: unknown[] }).items)
              assertEngineModel(model, item);
          return true;
        } catch {
          return false;
        }
      });
      setRuns(
        validPages
          .filter((page) => page.op === 'runs')
          .flatMap((page) => (page.value as { items?: EngineRun[] }).items ?? []),
      );
      setRequests(
        validPages
          .filter((page) => page.op === 'requests')
          .flatMap((page) => (page.value as { items?: EngineRequest[] }).items ?? []),
      );
      setArtifacts(
        validPages
          .filter((page) => page.op === 'artifacts')
          .flatMap((page) => (page.value as { items?: EngineArtifact[] }).items ?? []),
      );
      setPending(saved.pending);
      setInfo(info);
    } catch (error) {
      if (current === revision.current) {
        await disconnectEngine().catch(() => {});
        setError(engineError(error).message);
      }
    } finally {
      if (current === revision.current) setConnecting(false);
    }
  }
  async function disconnect() {
    revision.current++;
    await disconnectEngine();
    setInfo(undefined);
    setError('');
    setPending([]);
  }
  async function command<T>(call: EngineCall): Promise<T> {
    try {
      const response = await engineCall<T>(call);
      await refresh();
      return response;
    } catch (error) {
      const caught = engineError(error);
      notify(caught.message);
      throw caught;
    } finally {
      try {
        setPending((await engineCache()).pending);
      } catch {
        /* Already reported by the command. */
      }
    }
  }
  return {
    info,
    connecting,
    syncing,
    error,
    runs,
    requests,
    artifacts,
    profiles,
    resources,
    pending,
    connect,
    disconnect,
    refresh,
    command,
  };
}

export type EngineController = ReturnType<typeof useEngine>;
