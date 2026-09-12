-- Copyright 2026 Khuriwal Group
-- SPDX-License-Identifier: Apache-2.0

-- F47: ResourceVector JSONB was written by default protojson, which emits
-- camelCase (cpuMillis), while the schema comments, proto source, docs and all
-- operator SQL use snake_case (cpu_millis).
--
-- A JSONB key that does not exist yields SQL NULL rather than an error, so the
-- mismatch was silent: the capacity aggregation summed a key nothing had and
-- reported 0 for a fleet with real capacity. The writers now use UseProtoNames;
-- this migrates the rows already written.
--
-- Idempotent: rows already in snake_case are untouched, because the camelCase
-- keys they lack simply do not match.

UPDATE workers
   SET capacity = capacity
        - 'cpuMillis' - 'memoryBytes' - 'gpuCount' - 'gpuKind'
        - 'gpuMemoryBytes' - 'diskBytes'
        || jsonb_strip_nulls(jsonb_build_object(
             'cpu_millis',       capacity -> 'cpuMillis',
             'memory_bytes',     capacity -> 'memoryBytes',
             'gpu_count',        capacity -> 'gpuCount',
             'gpu_kind',         capacity -> 'gpuKind',
             'gpu_memory_bytes', capacity -> 'gpuMemoryBytes',
             'disk_bytes',       capacity -> 'diskBytes'))
 WHERE capacity ?| array['cpuMillis','memoryBytes','gpuCount','gpuKind','gpuMemoryBytes','diskBytes'];

UPDATE workers
   SET committed = committed
        - 'cpuMillis' - 'memoryBytes' - 'gpuCount' - 'gpuKind'
        - 'gpuMemoryBytes' - 'diskBytes'
        || jsonb_strip_nulls(jsonb_build_object(
             'cpu_millis',       committed -> 'cpuMillis',
             'memory_bytes',     committed -> 'memoryBytes',
             'gpu_count',        committed -> 'gpuCount',
             'gpu_kind',         committed -> 'gpuKind',
             'gpu_memory_bytes', committed -> 'gpuMemoryBytes',
             'disk_bytes',       committed -> 'diskBytes'))
 WHERE committed ?| array['cpuMillis','memoryBytes','gpuCount','gpuKind','gpuMemoryBytes','diskBytes'];
