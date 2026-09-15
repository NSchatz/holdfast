// What the page shows for a DRY RUN's recorded decisions - measured in a real engine,
// decided by pure functions.
//
// The split is the repository's own and it is what makes every grader here provable:
// `readCandidates` MEASURES and decides nothing, the graders below DECIDE and measure
// nothing, and the spec files drive the world. Because a grader is a pure function of one
// reading, `mutations.spec.mjs` can run each of them against a document deliberately built
// to defeat it and fail the run if the grader stays silent.
//
// Nothing here matches HTML or CSS source text. A text grader cannot decide what a rule
// applies to, what wins the cascade, or what is SHOWN rather than merely built.

// The state a dry run's decision is recorded under, spelled once.
export const CANDIDATE = "would-transcode";

// The `candidates` fixture, stated once, so a grader's expectation and the snapshot the
// server publishes cannot drift apart - and so the case that GRADES the shipped page and
// the case that DEFEATS each grader are deciding about the same world.
//
// Four candidates: two that recorded both facts, one that recorded neither, and one whose
// codec is an empty string and whose size arrived as a STRING rather than a number - the
// three shapes a fact can be absent in on this wire. The total is therefore the two real
// source sizes (8 MiB + 2 MiB) and the page must say it left two rows out.
export const CANDIDATES = {
  scenario: "candidates",
  chip: 4,
  rows: {
    "/mnt/media/movies/Dune Part Two (2024).mkv": { codec: "h264", size: "8.0 MB" },
    "/mnt/media/movies/Arrival (2016).mp4": { codec: "mpeg4", size: "2.0 MB" },
  },
  total: { figure: "10.0 MB", coverage: "over 4 candidate rows shown" },
  absent: {
    unrecordedCodec: [
      "/mnt/media/tv/Halcyon Drift/Season 01/S01E09.mkv",
      "/mnt/media/tv/Halcyon Drift/Season 01/S01E10.mkv",
    ],
    unrecordedSize: [
      "/mnt/media/tv/Halcyon Drift/Season 01/S01E09.mkv",
      "/mnt/media/tv/Halcyon Drift/Season 01/S01E10.mkv",
    ],
    exclusion: "2 rows excluded: no recorded size",
    stillRendered: [
      "/mnt/media/movies/Dune Part Two (2024).mkv",
      "/mnt/media/tv/Northwind/Season 02/S02E05.mkv",
      "/mnt/media/tv/Northwind/Season 02/S02E06.mkv",
    ],
  },
  spoken: {
    phrase: "4 would be transcoded",
    unchanged: ["1 done", "1 skipped", "0 failed"],
  },
};

// The page's one absence phrase.
export const NOT_RECORDED = "not recorded";

// Words that would make a candidate figure a PROJECTION. Nobody has encoded these files,
// so no honest number exists for what they would give back, and a figure that guessed
// would be read as a measurement - the same overclaim as rendering an unmeasured VMAF as
// 0.0, which this repository refuses on every other surface.
export const PROJECTION_WORDS = [
  "smaller", "saving", "would save", "projected", "estimate", "reclaim", "output",
];

// readCandidates takes ONE whole reading of what the engine rendered. Every field is
// something the engine computed: `innerText` for what a reader can read, the resolved
// colour behind each run of text, real layout geometry, and - for the selectable-text
// claim - what a Range over the cell's own contents actually yields.
export async function readCandidates(page) {
  return page.evaluate((candidate) => {
    function visible(el) {
      if (!el) return false;
      for (let n = el; n; n = n.parentElement) {
        const cs = getComputedStyle(n);
        if (cs.display === "none" || cs.visibility === "hidden" || Number(cs.opacity) === 0) return false;
        if (n.hasAttribute && n.hasAttribute("hidden")) return false;
      }
      const r = el.getBoundingClientRect();
      return r.width > 0 && r.height > 0;
    }
    function text(el) {
      return el ? String(el.innerText || "").replace(/\s+/g, " ").trim() : "";
    }
    // The colour actually PAINTED behind a run of text: a transparent background is not a
    // colour a reader sees, so the walk continues to the ancestor that paints one.
    function behind(el) {
      for (let n = el; n; n = n.parentElement) {
        const bg = getComputedStyle(n).backgroundColor;
        const m = bg.match(/rgba?\(([^)]+)\)/);
        if (!m) continue;
        const p = m[1].split(/[,\s/]+/).filter(Boolean).map(Number);
        if (p.length < 4 || p[3] > 0) return bg;
      }
      return getComputedStyle(document.body).backgroundColor;
    }
    // run is one measured run of text. A subject that is NOT THERE still comes back with
    // every field, because a grader has to REPORT an absence rather than throw on it: a
    // grader that throws is a grader whose verdict the runner turns into an error nobody
    // can read, and the whole point of the mutation half is that each one fails by SAYING
    // what it saw.
    function run(what, el) {
      if (!el) {
        return { what: what, present: false, shown: false, text: "", fg: "", bg: "", size: 0, weight: 400, box: { w: 0, h: 0 } };
      }
      const cs = getComputedStyle(el);
      const r = el.getBoundingClientRect();
      return {
        what: what, present: true, shown: visible(el), text: text(el),
        fg: cs.color, bg: behind(el),
        size: parseFloat(cs.fontSize) || 0, weight: Number(cs.fontWeight) || 400,
        box: { w: r.width, h: r.height },
      };
    }
    // What a reader can actually SELECT out of one element: a real Range over its own
    // contents, which is what a copy gesture produces. Generated content (a ::before) and
    // a subtree the cascade has taken out of selection yield nothing here, which is the
    // difference between text a reader can take away and a picture of it.
    function selectable(el) {
      if (!el) return "";
      for (let n = el; n; n = n.parentElement) {
        const us = getComputedStyle(n);
        if ((us.userSelect || us.webkitUserSelect) === "none") return "";
      }
      const range = document.createRange();
      range.selectNodeContents(el);
      return String(range.toString()).replace(/\s+/g, " ").trim();
    }

    const chips = [];
    const host = document.getElementById("chips");
    if (host) {
      for (const c of host.children) {
        const n = c.querySelector(".n"), k = c.querySelector(".k");
        chips.push({
          key: text(k),
          count: text(n),
          terminal: c.classList.contains("terminal"),
          inflight: c.classList.contains("inflight"),
          shown: visible(c),
          number: run("the count of the " + text(k) + " chip", n),
          label: run("the key of the " + text(k) + " chip", k),
        });
      }
    }

    const rows = [];
    for (const tr of document.querySelectorAll("#history tr")) {
      if (tr.dataset && tr.dataset.state) continue;
      const st = tr.querySelector("td.st"), size = tr.querySelector("td.size");
      const holder = st ? st.querySelector("[class*=st-]") : null;
      const m = holder ? /st-([a-z-]+)/.exec(holder.getAttribute("class") || "") : null;
      const codecEl = st ? st.querySelector(".cond") : null;
      // The path is read from the cell's CHARACTER DATA rather than from innerText. The
      // cell renders the directory and the file's own name as two blocks - which is what
      // makes a column of paths readable - and innerText puts a line break between two
      // blocks, so the path a reader copies out is the concatenation and never the two
      // with a separator between them. This is the identity a case names a row by, so it
      // has to be the path the server sent, character for character.
      const pathCell = tr.querySelector("td.path");
      rows.push({
        path: pathCell ? String(pathCell.textContent || "").trim() : "",
        status: m ? m[1] : "",
        shown: visible(tr),
        rowText: text(tr),
        result: run("the result cell of a " + (m ? m[1] : "?") + " row", st),
        size: run("the size cell of a " + (m ? m[1] : "?") + " row", size),
        codec: run("the source-codec line of a " + (m ? m[1] : "?") + " row", codecEl),
        codecSelected: selectable(codecEl),
        sizeSelected: selectable(size),
        // Whether the two cells render the page's own absence phrase rather than a blank
        // or a zero. Read from the ELEMENT the page builds for an absence, so a cell that
        // merely happened to contain those words in prose is not mistaken for one.
        codecAbsent: !!(codecEl && codecEl.querySelector(".nr")),
        sizeAbsent: !!(size && size.querySelector(".nr")),
      });
    }

    const block = document.getElementById("cand");
    const total = {
      present: !!block,
      shown: visible(block),
      text: text(block),
      figure: run("the total under consideration", document.getElementById("cand-bytes")),
      label: run("the label of the total under consideration", block ? block.querySelector(".cand-k") : null),
      coverage: run("the set the total under consideration is over", document.getElementById("cand-cov")),
      exclusion: run("the rows the total under consideration left out", document.getElementById("cand-ex")),
      figureAbsent: !!(block && block.querySelector("#cand-bytes .nr")),
    };

    const sr = document.getElementById("sr-status");
    return {
      candidate: candidate,
      chips: chips,
      rows: rows,
      candidateRows: rows.filter(function (r) { return r.status === candidate; }),
      total: total,
      announcement: text(sr),
      bodyText: String(document.body.innerText || "").replace(/\s+/g, " ").trim(),
      viewportWidth: window.innerWidth,
    };
  }, CANDIDATE);
}

// --- WCAG 2.2's own relative luminance, recomputed here rather than trusted -----------
const RGB = /rgba?\(\s*([\d.]+)[,\s]+([\d.]+)[,\s]+([\d.]+)/;

export function contrastOf(fg, bg) {
  const lum = (s) => {
    const m = RGB.exec(String(s).trim());
    if (!m) return null;
    const acc = [0, 0, 0];
    for (let i = 0; i < 3; i++) {
      const n = Number(m[i + 1]);
      if (!isFinite(n)) return null;
      const v = n / 255;
      acc[i] = v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
    }
    return 0.2126 * acc[0] + 0.7152 * acc[1] + 0.0722 * acc[2];
  };
  const a = lum(fg), b = lum(bg);
  if (a === null || b === null) return null;
  const hi = Math.max(a, b), lo = Math.min(a, b);
  return (hi + 0.05) / (lo + 0.05);
}

// legible is WCAG 2.2's text floor applied to one measured run: 4.5:1, relaxed to 3:1 for
// large text. It is the same arithmetic the page-wide contrast grader uses, applied here
// to the runs this criterion is about so that a failure names them.
function legible(r) {
  const out = [];
  if (!r.present) return [r.what + " is not in the document"];
  if (!r.shown) return [r.what + " is in the document but never reached the screen"];
  if (r.text === "") out.push(r.what + " renders no text at all");
  const floor = (r.size >= 24 || (r.size >= 18.66 && r.weight >= 700)) ? 3.0 : 4.5;
  const ratio = contrastOf(r.fg, r.bg);
  if (ratio === null) {
    out.push(r.what + ": the engine reported an unreadable colour pair (" + r.fg + " on " + r.bg + ")");
  } else if (ratio < floor - 0.005) {
    out.push(r.what + ' ("' + r.text + '") is ' + ratio.toFixed(2) + ":1 (" + r.fg + " on " + r.bg +
      "), under the " + floor.toFixed(1) + ":1 floor");
  }
  return out;
}

// AC15. The count is its own VISIBLE figure, it is drawn with the terminal states and not
// with the in-flight ones, and its text clears the contrast floor in the theme under test.
//
// The grouping half is the one that matters beyond tidiness: the page draws work IN HAND
// apart from work FINISHED, and a decided candidate shown among the in-flight counts would
// say a worker is examining a file nothing is examining - which is exactly the misreading
// the whole state exists to end.
export function gradeCandidateCountIsItsOwnTerminalFigure(s, want) {
  const out = [];
  const chip = s.chips.find((c) => c.key === s.candidate);
  if (!chip) {
    return ["the summary renders no chip for " + s.candidate + ", so a dry run's decisions " +
      "have no figure of their own at all (chips: " + s.chips.map((c) => c.key).join(", ") + ")"];
  }
  if (!chip.shown) out.push("the " + s.candidate + " chip is in the document but never reached the screen");
  if (want !== undefined && chip.count !== String(want)) {
    out.push("the " + s.candidate + " chip reads " + JSON.stringify(chip.count) + ", want " + JSON.stringify(String(want)));
  }
  if (!chip.terminal) {
    out.push("the " + s.candidate + " chip is not drawn with the terminal states");
  }
  if (chip.inflight) {
    out.push("the " + s.candidate + " chip is drawn with the WORK IN HAND; the decision is taken " +
      "and nothing is examining that file");
  }
  // The state is spelled out beside its number, so the count is never carried by position
  // or colour alone.
  if (chip.label.text !== s.candidate) {
    out.push("the chip's key reads " + JSON.stringify(chip.label.text) + ", want the state in words (" + s.candidate + ")");
  }
  out.push(...legible(chip.number), ...legible(chip.label));
  return out;
}

// AC16. Every rendered candidate row shows the source's codec and the source's size, as
// text a reader can SELECT - proved by a real Range over the cell's own contents, which is
// what a copy gesture yields, so generated content or an unselectable subtree fails here.
export function gradeEveryCandidateRowShowsItsCodecAndSize(s, expected) {
  const out = [];
  if (s.candidateRows.length === 0) {
    return ["the page rendered no " + s.candidate + " row at all, so this grader decided nothing"];
  }
  // Every row this case NAMED has to be on the page. Iterating the rendered rows alone
  // would pass vacuously the moment a path stopped matching - which is exactly how a
  // grader stops deciding while still reporting "ok".
  for (const path of Object.keys(expected || {})) {
    if (!s.candidateRows.some((r) => r.path === path)) {
      out.push("the candidate row " + path + " is not on the page at all (rendered: " +
        s.candidateRows.map((r) => r.path).join(", ") + ")");
    }
  }
  for (const row of s.candidateRows) {
    const want = expected && expected[row.path];
    if (!want) continue; // a row this case did not name; the absence grader covers those
    if (!row.shown) { out.push("the candidate row " + row.path + " never reached the screen"); continue; }
    for (const [what, run, selected, value] of [
      ["source codec", row.codec, row.codecSelected, want.codec],
      ["source size", row.size, row.sizeSelected, want.size],
    ]) {
      out.push(...legible(run));
      if (!run.text.includes(value)) {
        out.push(row.path + ": the " + what + " reads " + JSON.stringify(run.text) + ", want it to carry " + JSON.stringify(value));
      }
      if (!selected.includes(value)) {
        out.push(row.path + ": the " + what + " (" + JSON.stringify(value) + ") is not text a reader can select; " +
          "a Range over the cell's own contents yields " + JSON.stringify(selected));
      }
    }
  }
  return out;
}

// AC17. The total is the SOURCE bytes those candidates account for, labelled as what is
// under consideration, stating the set it was taken over - and nowhere on the page is
// there a projected saving, output size or reclaim for a file nothing has encoded.
export function gradeTheTotalUnderConsiderationIsSourceBytesAndNoProjection(s, want) {
  const out = [];
  if (!s.total.present) return ["the page carries no total-under-consideration block at all"];
  if (!s.total.shown) {
    return ["the total under consideration is in the document but never reached the screen, " +
      "so the candidates add up to nothing a reader can see"];
  }
  out.push(...legible(s.total.figure), ...legible(s.total.label), ...legible(s.total.coverage));
  if (want && want.figure !== undefined && !s.total.figure.text.includes(want.figure)) {
    out.push("the total under consideration reads " + JSON.stringify(s.total.figure.text) +
      ", want it to carry " + JSON.stringify(want.figure) + " - the SOURCE bytes of the candidate rows");
  }
  if (!/under consideration/i.test(s.total.label.text)) {
    out.push("the total is labelled " + JSON.stringify(s.total.label.text) +
      ", which does not say it is the size of what is under consideration");
  }
  // Clause F4: a figure computed over rows says WHICH rows, beside the figure.
  if (want && want.coverage !== undefined && !s.total.coverage.text.includes(want.coverage)) {
    out.push("the set the total is over reads " + JSON.stringify(s.total.coverage.text) +
      ", want it to carry " + JSON.stringify(want.coverage));
  }
  // No projection, anywhere a reader meets these files: not in the block, not on a row.
  const surfaces = [["the total block", s.total.text]];
  for (const r of s.candidateRows) surfaces.push(["the candidate row " + r.path, r.rowText]);
  for (const [what, body] of surfaces) {
    for (const word of PROJECTION_WORDS) {
      if (body.toLowerCase().includes(word)) {
        out.push(what + " carries " + JSON.stringify(word) + ": " + JSON.stringify(body) +
          ". Nothing has encoded these files, so a projected saving, output size or reclaim " +
          "would be read as a measurement of one");
      }
    }
  }
  return out;
}

// AC19. A candidate whose codec or size was never recorded renders as NOT RECORDED - never
// as a zero and never as a blank - is left OUT of the total, the page states that it was,
// and every other row and figure still renders.
export function gradeAnUnrecordedFactReadsAsNotRecordedAndIsExcluded(s, want) {
  const out = [];
  for (const path of (want && want.unrecordedCodec) || []) {
    const row = s.candidateRows.find((r) => r.path === path);
    if (!row) { out.push("the row " + path + " is not on the page, so its absence proves nothing"); continue; }
    if (!row.codecAbsent) {
      out.push(path + ": a source codec nobody recorded renders as " + JSON.stringify(row.codec.text) +
        ', want the page\'s own absence phrase ("' + NOT_RECORDED + '")');
    }
    if (/\b0\b/.test(row.codec.text)) out.push(path + ": an unrecorded codec renders a zero: " + JSON.stringify(row.codec.text));
  }
  for (const path of (want && want.unrecordedSize) || []) {
    const row = s.candidateRows.find((r) => r.path === path);
    if (!row) { out.push("the row " + path + " is not on the page, so its absence proves nothing"); continue; }
    if (!row.sizeAbsent) {
      out.push(path + ": a source size nobody recorded renders as " + JSON.stringify(row.size.text) +
        ', want the page\'s own absence phrase ("' + NOT_RECORDED + '")');
    }
    if (/(^|[^0-9.])0( ?B|[^0-9.]|$)/.test(row.size.text)) {
      out.push(path + ": an unrecorded size renders as a zero (" + JSON.stringify(row.size.text) +
        "), which a reader adds up");
    }
  }
  // Excluded from the total, and SAID to be.
  if (want && want.exclusion !== undefined) {
    out.push(...legible(s.total.exclusion));
    if (!s.total.exclusion.text.includes(want.exclusion)) {
      out.push("the total under consideration says " + JSON.stringify(s.total.exclusion.text) +
        " about the rows it left out, want it to carry " + JSON.stringify(want.exclusion) +
        " - a row dropped in silence is a total that reads as covering everything");
    }
  }
  // F5: one unreadable figure costs nothing else. Every other row is still on the page.
  for (const path of (want && want.stillRendered) || []) {
    const row = s.rows.find((r) => r.path === path);
    if (!row || !row.shown) {
      out.push("the row " + path + " is missing from the page; an unrecorded fact on one row " +
        "must cost nothing else");
    }
  }
  return out;
}

// AC18. A snapshot in which nothing was decided that way shows the count as a REAL ZERO,
// no candidate rows, and NO total in place of one it has no rows for.
//
// The last limb is the one with teeth. "0 B under consideration" beside an empty candidate
// list is a claim about files nobody has looked at, and it is exactly what a figure written
// to always render would say.
export function gradeNoCandidateFigureWhenNothingWasDecided(s) {
  const out = [];
  const chip = s.chips.find((c) => c.key === s.candidate);
  if (!chip) {
    out.push("the summary renders no chip for " + s.candidate + "; a state with nothing in it " +
      "still has a measured count of zero, and the page's own summary reports it");
  } else {
    if (chip.count !== "0") {
      out.push("the " + s.candidate + " chip reads " + JSON.stringify(chip.count) + " on a snapshot with no such row, want a real 0");
    }
    if (!chip.shown) out.push("the " + s.candidate + " chip never reached the screen");
    out.push(...legible(chip.number), ...legible(chip.label));
  }
  if (s.candidateRows.length !== 0) {
    out.push("the page renders " + s.candidateRows.length + " candidate row(s) for a snapshot that carries none");
  }
  if (s.total.shown) {
    out.push("the total under consideration is on screen with no candidate row to total: " +
      JSON.stringify(s.total.text) + ". A figure over an empty set is a claim about files nobody has looked at");
  }
  return out;
}

// AC20. The screen-reader announcement speaks the count as its own named figure and does
// not fold it into any other state's count.
export function gradeTheAnnouncementNamesTheCandidateCount(s, want) {
  const out = [];
  const said = s.announcement;
  if (!said) return ["the page announced nothing at all to a screen reader"];
  if (!said.includes(want.phrase)) {
    out.push("the announcement is " + JSON.stringify(said) + ", which does not speak the candidate " +
      "count as its own figure (" + JSON.stringify(want.phrase) + ")");
  }
  // And the other counts are unchanged by it: folding candidates into done, skipped or
  // failed would be the same lie the page refuses on every other surface.
  for (const other of want.unchanged || []) {
    if (!said.includes(other)) {
      out.push("the announcement is " + JSON.stringify(said) + ", which no longer carries " +
        JSON.stringify(other) + " - a candidate count folded into another state's is a count " +
        "a listener cannot separate");
    }
  }
  return out;
}

// candidateGraders is every predicate this file decides, by name, with the reading and
// expectations one scenario supplies. The mutation spec drives each one against a document
// built to defeat it, so a grader that stopped deciding cannot report "ok".
export function candidateGraders(want) {
  return [
    {
      name: "the candidate count is its own figure, drawn with the terminal states",
      probe: (s) => gradeCandidateCountIsItsOwnTerminalFigure(s, want.chip),
    },
    {
      name: "every candidate row shows its source codec and source size, selectably",
      probe: (s) => gradeEveryCandidateRowShowsItsCodecAndSize(s, want.rows),
    },
    {
      name: "the total under consideration is source bytes and names no projection",
      probe: (s) => gradeTheTotalUnderConsiderationIsSourceBytesAndNoProjection(s, want.total),
    },
    {
      name: "an unrecorded codec or size reads as not recorded and is excluded",
      probe: (s) => gradeAnUnrecordedFactReadsAsNotRecordedAndIsExcluded(s, want.absent),
    },
    {
      name: "the announcement names the candidate count",
      probe: (s) => gradeTheAnnouncementNamesTheCandidateCount(s, want.spoken),
    },
  ];
}
