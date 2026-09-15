// What the dashboard shows for a DRY RUN's recorded decisions.
//
// An operator turns dry_run on to answer one question before they let this tool delete
// anything: which files would it transcode? Every criterion here is about what that page
// SHOWS them, so every one is decided by reading what a real engine rendered - computed
// style after the whole cascade, real layout geometry, the text `innerText` says a reader
// can see, and a real Range for the text they can select.
//
// This file runs under EVERY project the harness declares - dark at 1440, light at 1440
// and dark at 360 - so "legible in both themes at the widths the harness exercises" is
// decided once per world rather than asserted of one.
import { test, expect } from "@playwright/test";
import { open } from "./dashboard.mjs";
import {
  CANDIDATES,
  readCandidates,
  gradeCandidateCountIsItsOwnTerminalFigure,
  gradeEveryCandidateRowShowsItsCodecAndSize,
  gradeTheTotalUnderConsiderationIsSourceBytesAndNoProjection,
  gradeAnUnrecordedFactReadsAsNotRecordedAndIsExcluded,
  gradeNoCandidateFigureWhenNothingWasDecided,
  gradeTheAnnouncementNamesTheCandidateCount,
} from "./candidates.mjs";

async function candidateReading(page, scenario = CANDIDATES.scenario) {
  const problems = await open(page, scenario);
  const s = await readCandidates(page);
  expect(problems, problems.join("\n")).toEqual([]);
  return s;
}

// AC15.
test("the candidate count is its own visible figure, drawn with the finished states", async ({ page }) => {
  const s = await candidateReading(page);
  const probs = gradeCandidateCountIsItsOwnTerminalFigure(s, CANDIDATES.chip);
  expect(probs, probs.join("\n")).toEqual([]);
});

// AC16.
test("every candidate row shows its source codec and its source size, as selectable text", async ({ page }) => {
  const s = await candidateReading(page);
  const probs = gradeEveryCandidateRowShowsItsCodecAndSize(s, CANDIDATES.rows);
  expect(probs, probs.join("\n")).toEqual([]);
});

// AC17.
test("the candidates total their SOURCE bytes and the page projects no saving", async ({ page }) => {
  const s = await candidateReading(page);
  const probs = gradeTheTotalUnderConsiderationIsSourceBytesAndNoProjection(s, CANDIDATES.total);
  expect(probs, probs.join("\n")).toEqual([]);
});

// AC19.
test("a candidate whose codec or size was never recorded says so and is left out of the total", async ({ page }) => {
  const s = await candidateReading(page);
  const probs = gradeAnUnrecordedFactReadsAsNotRecordedAndIsExcluded(s, CANDIDATES.absent);
  expect(probs, probs.join("\n")).toEqual([]);
});

// AC18. The `full` scenario is the stronger empty case: a page with rows, figures and a
// populated history in which NOTHING was decided that way. A snapshot with nothing in it
// at all could pass this by rendering nothing anywhere.
test("a snapshot with no candidate shows a real zero, no rows and no total", async ({ page }) => {
  const s = await candidateReading(page, "full");
  const probs = gradeNoCandidateFigureWhenNothingWasDecided(s);
  expect(probs, probs.join("\n")).toEqual([]);
});

// AC20.
test("the screen-reader summary speaks the candidate count as its own named figure", async ({ page }) => {
  const s = await candidateReading(page);
  const probs = gradeTheAnnouncementNamesTheCandidateCount(s, CANDIDATES.spoken);
  expect(probs, probs.join("\n")).toEqual([]);
});
