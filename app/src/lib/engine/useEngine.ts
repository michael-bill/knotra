import { assertEngineModel } from './validation';
import { useCallback, useEffect, useRef, useState } from 'react';
import {
  allPages,
  connectEngine,
  disconnectEngine,
  engineCache,
  engineCall,
  engineError,
  EngineError,
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
  const refreshing = useRef<
    { revision: number; again: boolean; promise: Promise<void> } | undefined
  >(undefined);
  const refresh = useCallback(
    async (queue = true): Promise<void> => {
      const current = revision.current;
      if (!info) return;
      if (refreshing.current?.revision === current) {
        // A command or event received during a read needs another read after it.
        if (queue) refreshing.current.again = true;
        return refreshing.current.promise;
      }
      const task = { revision: current, again: true, promise: Promise.resolve() };
      refreshing.current = task;
      setSyncing(true);
      task.promise = (async () => {
        // Coalesce at most one follow-up; a continuous event stream cannot keep
        // one refresh alive indefinitely.
        for (let pass = 0; pass < 2; pass++) {
          task.again = false;
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
          }
          if (!task.again || current !== revision.current) break;
        }
      })().finally(() => {
        if (refreshing.current === task) refreshing.current = undefined;
        if (current === revision.current) {
          setSyncing(false);
          // Events/commands during the final read still need a later snapshot;
          // start it independently after releasing this bounded task.
          if (task.again) void refresh(false);
        }
      });
      return task.promise;
    },
    [info],
  );
  useEffect(() => {
    void refresh();
    if (!info) return;
    const timer = setInterval(() => void refresh(false), 10000);
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
      if (current !== revision.current) return;
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
    const current = ++revision.current;
    await disconnectEngine();
    if (current !== revision.current) return;
    setInfo(undefined);
    setError('');
    setPending([]);
  }
  async function command<T>(call: EngineCall): Promise<T> {
    const current = revision.current;
    let response: T;
    try {
      response = await engineCall<T>(call);
      // The durable receipt is already confirmed. A slow/unavailable read must
      // not keep the form busy or prevent the caller from receiving it.
      if (current === revision.current) void refresh();
    } catch (error) {
      const caught = engineError(error);
      if (current === revision.current) notify(caught.message);
      throw caught;
    } finally {
      try {
        const saved = await engineCache();
        if (current === revision.current) setPending(saved.pending);
      } catch {
        /* Already reported by the command. */
      }
    }
    if (current !== revision.current)
      throw new EngineError(
        'disconnected',
        'Connection changed; recover commands on the original engine.',
      );
    return response;
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
