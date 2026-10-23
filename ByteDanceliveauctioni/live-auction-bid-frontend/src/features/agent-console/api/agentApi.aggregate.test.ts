import { describe, expect, it } from 'vitest';
import { decodeSessionAggregate } from './agentApi';

describe('decodeSessionAggregate', () => {
  it('decodes the real Go aggregate acronym fields returned after a completed run', () => {
    const aggregate = decodeSessionAggregate({
      session: {
        id: 'session-1', roomId: 'room-1', status: 'COLLECTING_INPUT', version: 3,
        activeSnapshotVersion: 0, conversationSummary: '', createdAtUnixMs: 1, updatedAtUnixMs: 2,
      },
      messages: [{
        SessionID: 'session-1', Seq: 1, Role: 'USER', Content: 'create one lot',
        RunID: 'run-1', CreatedAtUnixMs: 1,
      }],
      candidates: [{
        Candidate: {
          id: 'candidate-1', sourceType: 'NEW', startPriceSpecified: true,
          draft: {
            title: 'Ocean Heart', startPriceAmount: 1_000_000, durationSeconds: 3_000,
            mainImageAssetId: '', mainImageUrl: '', minIncrementAmount: 10_000,
            capPriceAmount: 10_000_000, category: 'jewelry', description: '', tags: [],
            stock: 1, antiSnipeWindowSeconds: 30, antiSnipeExtendSeconds: 10,
            maxExtendCount: 3, depositAmount: 0,
          },
        },
        Position: 1, SelectedByDefault: true, FieldSourcesJSON: '{}', ProvenanceVersion: 1,
        ValidationJSON: '{"requiredInputs":["MAIN_IMAGE"]}', Version: 1,
        CreatedAtUnixMs: 1, UpdatedAtUnixMs: 2,
      }],
      activeRun: {
        RunID: 'run-1', Goal: 'create one lot', Status: 'COMPLETED',
        CreatedAtUnixMs: 1, UpdatedAtUnixMs: 2,
      },
    });

    expect(aggregate.messages[0]).toMatchObject({ sessionId: 'session-1', runId: 'run-1' });
    expect(aggregate.candidates[0].candidate.id).toBe('candidate-1');
    expect(aggregate.session.version).toBe(3);
    expect(aggregate.activeRun?.runId).toBe('run-1');
  });
});
