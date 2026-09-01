// The state chip both screens paint. Unknown is what the floor shows when
// it has no source, and it never softens to idle here.

export const ORDER = { blocked: 0, working: 1, unknown: 2, idle: 3, done: 4 };
export const WORDS = ['blocked', 'working', 'idle', 'done', 'unknown'];

export function stateChip(state) {
  const word = WORDS.includes(state) ? state : 'unknown';
  return { word, cls: 'chip chip-' + word };
}
