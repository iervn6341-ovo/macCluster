#!/usr/bin/env python3
"""Prints the hitches and hang risks of an Instruments 'Animation Hitches' trace as plain text.

    summarize-hitches.py <file.trace>

For each hitch: when it started, how long it lasted and what the system says about it (for example "Expensive
commit" or "Expensive app update"), so a stall can be tied to the app's main thread or to the render server."""
import re, subprocess, sys, collections, xml.etree.ElementTree as ET

def table(trace, schema):
    xpath = f'/trace-toc/run[@number="1"]/data/table[@schema="{schema}"]'
    out = subprocess.run(["xcrun", "xctrace", "export", "--input", trace, "--xpath", xpath],
                         capture_output=True, text=True, timeout=180).stdout
    if "<schema" not in out:
        return [], []
    root = ET.fromstring(out)
    cols = [c.findtext("mnemonic") or "?" for c in root.iter("col")]
    ids = {}
    rows = []
    for row in root.iter("row"):
        cells = []
        for cell in row:
            if "ref" in cell.attrib:
                cells.append(ids.get(cell.attrib["ref"], ""))
            else:
                text = cell.attrib.get("fmt") or (cell.text or "")
                if "id" in cell.attrib:
                    ids[cell.attrib["id"]] = text
                cells.append(text)
        rows.append(cells)
    return cols, rows

def show(trace, schema, limit=25):
    cols, rows = table(trace, schema)
    print(f"== {schema}: {len(rows)} rows")
    dur = cols.index("duration") if "duration" in cols else None
    if dur is not None:
        def seconds(v):
            try:
                n, unit = v.split()
                return float(n) * {"s": 1, "ms": 1e-3, "µs": 1e-6, "us": 1e-6, "ns": 1e-9}.get(unit, 1)
            except Exception:
                return 0
        rows = sorted(rows, key=lambda r: -seconds(r[dur]))
    for r in rows[:limit]:
        print("  " + "  ".join(f"{c}={v}" for c, v in zip(cols, r) if v))

def clock_ns(text):
    """'00:05.826.050' (mm:ss.mmm.uuu) -> nanoseconds."""
    m = re.match(r"(?:(\d+):)?(\d+)\.(\d+)(?:\.(\d+))?", text or "")
    if not m:
        return None
    mins, secs, ms, us = (int(x) if x else 0 for x in m.groups())
    return ((mins * 60 + secs) * 1000 + ms) * 1_000_000 + us * 1000

def main_thread_profile(trace):
    """Main-thread Time Profiler samples as (time_ns, [frame names, leaf first], [binary names])."""
    xpath = '/trace-toc/run[@number="1"]/data/table[@schema="time-profile"]'
    out = subprocess.run(["xcrun", "xctrace", "export", "--input", trace, "--xpath", xpath],
                         capture_output=True, text=True, timeout=300).stdout
    if "<row>" not in out:
        return []
    root = ET.fromstring(out)
    frames, binaries, samples, threads = {}, {}, [], {}
    for row in root.iter("row"):
        t = row.find("sample-time")
        th = row.find("thread")
        if th is not None and "id" in th.attrib:
            threads[th.attrib["id"]] = th.attrib.get("fmt", "")
        label = th.attrib.get("fmt", "") if th is not None and "fmt" in th.attrib else threads.get(th.attrib.get("ref", ""), "") if th is not None else ""
        bt = row.find("tagged-backtrace")
        if t is None or bt is None or not label.startswith("Main Thread"):
            continue
        names, bins = [], []
        for fr in bt.iter("frame"):
            if "name" in fr.attrib:
                frames[fr.attrib["id"]] = fr.attrib["name"]
                b = fr.find("binary")
                if b is not None and "name" in b.attrib:
                    binaries[b.attrib["id"]] = b.attrib["name"]
                    bins.append(b.attrib["name"])
                elif b is not None and b.attrib.get("ref") in binaries:
                    bins.append(binaries[b.attrib["ref"]])
                else:
                    bins.append("")
                names.append(fr.attrib["name"])
            elif "ref" in fr.attrib and fr.attrib["ref"] in frames:
                names.append(frames[fr.attrib["ref"]])
                bins.append("")
        try:
            samples.append((int(t.text), names, bins))
        except (TypeError, ValueError):
            pass
    return samples

def profile_during_hitches(trace, limit=60):
    samples = main_thread_profile(trace)
    if not samples:
        print("== time profile: none recorded")
        return
    cols, rows = table(trace, "hitches")
    windows = []
    si, di = (cols.index("start"), cols.index("duration")) if "start" in cols and "duration" in cols else (None, None)
    for r in rows if si is not None else []:
        start = clock_ns(r[si])
        try:
            n, unit = r[di].split()
            length = float(n) * {"s": 1e9, "ms": 1e6, "µs": 1e3, "us": 1e3}.get(unit, 1e6)
        except Exception:
            continue
        if start is not None:
            windows.append((start - 25_000_000, start + length))   # the update that was late started before the hitch
    picked = [s for s in samples if any(a <= s[0] <= b for a, b in windows)] if windows else samples
    scope = f"{len(picked)} main-thread samples inside {len(windows)} hitch windows" if windows else "the whole recording (no hitches)"
    print(f"== main thread while frames were late: {scope}")
    inclusive = collections.Counter()
    leaf = collections.Counter()
    for _, names, bins in picked:
        inclusive.update(set(n for n in names if n))
        if names:
            leaf[names[0]] += 1
    print("-- most time inside (inclusive, ms ~ samples):")
    # Run-loop and layout plumbing says "the main thread was busy laying out", not what; leave it out so the
    # frames that name the views and the work (SwiftUI, our own code) are the ones listed.
    skip = re.compile(r"^(start|main$|NSApplicationMain|-\[NSApplication|_CFRunLoop|__CFRunLoop|CFRunLoop|RunCurrentEventLoop|ReceiveNextEvent|_DPSNextEvent|"
                      r"_DPSBlock|_BlockUntil|UC::|stepTransactionFlush|CA::Transaction|NSDisplayCycle|__NSWindowGetDisplayCycle|NSPerformVisuallyAtomicChange|"
                      r"-\[NSWindow\(NSConstraintBasedLayoutInternal\)|-\[NSView _layoutSubtree|-\[NSView layoutSubtree|__\d*\d*-?\[NSView|__36-|__56-|_NSViewLayout|___NSViewLayout|"
                      r"runApp|static App.main|specialized runApp|0x[0-9a-f]+|\?\?\?)")
    shown = 0
    for name, n in inclusive.most_common(800):
        if skip.match(name):
            continue
        print(f"  {n:5d}  {name[:150]}")
        shown += 1
        if shown >= limit:
            break
    print("-- where the CPU actually was (leaf):")
    for name, n in leaf.most_common(15):
        print(f"  {n:5d}  {name[:150]}")

if __name__ == "__main__":
    for schema in ("hitches", "hang-risks", "hitches-updates", "hitches-renders"):
        show(sys.argv[1], schema)
    profile_during_hitches(sys.argv[1])
