// Build a small, frozen catalogue using the original ModelTrace challenge generator.
// The service accepts only these IDs; the browser cannot submit arbitrary upstream prompts.
import { readFileSync, writeFileSync } from 'node:fs';
const js = readFileSync(new URL('../web/challenge-browser.js', import.meta.url));
const { generateChallenges } = await import('data:text/javascript;base64,'+js.toString('base64'));
const catalogue = Array.from({length:32}, () => generateChallenges(3));
writeFileSync(new URL('../questions.json', import.meta.url), JSON.stringify(catalogue));
