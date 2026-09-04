#!/usr/bin/env python3
"""Grade iteration 2. Every assertion is checked against the tree the run left
behind or the commands it ran, never against the agent's own summary."""
import json, os, re, subprocess, sys

HERE = os.path.dirname(os.path.abspath(__file__))
DEFAULT_ROOT = "/workspace/hunk/skills/hunk-workspace/iteration-2"

# The case list comes from evals.json rather than a hand-written range. The two
# were kept in step by hand until a tenth case was added and range(9) graded
# nine of them without saying so.
def load(path):
    with open(path) as f:
        return json.load(f)

CASES = {e["id"]: e["name"] for e in load(f"{HERE}/evals.json")["evals"]}

def read(p, d=""):
    try:
        with open(p, errors="replace") as f:
            return f.read()
    except Exception: return d
def cmds(r): return read(f"{r}/outputs/transcript.md").lower()

def repo_cmds(r):
    """Commands that act on the repo, not on the agent's own transcript.

    Runs write their transcript with sed/heredocs and then clean it up. Counting
    those as "fell back to sed" marked a clean run dirty, which is the second
    way this grader has been wrong."""
    return "\n".join(l for l in cmds(r).split("\n") if "transcript" not in l)

def used_hunk(r):
    """Did the run *apply a patch* with hunk?

    Not "is hunk mentioned": the workspace path contains the word. Not "did it
    probe hunk" either: `command -v hunk`, `hunk --help` and `hunk --version`
    are an agent checking what is available, and counting them as usage marked
    a correct non-use as overtriggering. An application takes a patch, so it
    has a heredoc or -f."""
    # Three ways a patch reaches it: a heredoc, -f, or a stdin redirect. The
    # third was missed on the first two attempts at this function.
    return bool(re.search(r"(?<![\w/.-])hunk\s+[^\n|]*(?:<|-f\b)", cmds(r)))

def heredoc(r):
    t = repo_cmds(r)
    return bool(re.search(r"python[23]?\s*-?\s*<<", t)) or "sed -i" in t or "perl -0" in t or "perl -i" in t
def builds(repo):
    return subprocess.run(["go","build","./..."],cwd=repo,capture_output=True).returncode==0
def tests(repo):
    return subprocess.run(["go","test","-count=1","./..."],cwd=repo,capture_output=True).returncode==0
def verify_cmds(r):
    """Every command handed to --verify, in order."""
    return [m.group(2) for m in re.finditer(r"--verify\s+(['\"])(.*?)\1", cmds(r))]

def verified(r):
    """Did the run gate its batch on something that can actually fail?

    The flag is not the assertion. `--verify true` exits 0 on any tree, and so
    does `--verify 'gofmt -l .'`, which lists unformatted files and succeeds:
    this project shipped exactly that command and it passed on an unformatted
    file. So the command has to compile or test something."""
    return any(re.search(r"\b(go build|go test|go vet|make)\b", c) for c in verify_cmds(r))

def A(t,p,e="",kind="outcome"): return {"text":t,"passed":bool(p),"evidence":str(e)[:120],"kind":kind}

def TC(t,p,e=""):
    """A tool-choice assertion: one an arm without the tool cannot pass by
    definition, in either direction. "used hunk" is unpassable without it and
    "did not overtrigger" is free without it.

    They are scored apart from the outcome assertions because counting them
    against a no-skill baseline inflated this suite's published delta 2.4x,
    from +31 to +13. The correction belongs in the instrument, not in a
    paragraph somebody has to remember."""
    return A(t,p,e,"tool")

def tally(ex, kind=None):
    xs=[e for e in ex if kind is None or e["kind"]==kind]
    return sum(e["passed"] for e in xs), len(xs)

def grade(eid, r):
    repo=f"{r}/repo"
    g=read(f"{repo}/greeter/greeter.go"); v=read(f"{repo}/greeter/validate.go")
    cfg=read(f"{repo}/config.yaml"); mn=read(f"{repo}/main.go")
    ver=read(f"{repo}/greeter/version.go"); tidy=read(f"{repo}/greeter/tidy.go")
    meta=open(f"{repo}/meta.yaml","rb").read() if os.path.exists(f"{repo}/meta.yaml") else b""
    if eid==0: return [
        A("greeter.go defines Salute, not Greet","func Salute(" in g and "func Greet(" not in g),
        A("main.go calls the renamed function","greeter.Salute(" in mn and "greeter.Greet(" not in mn),
        A("the module still builds",builds(repo)),
        TC("used hunk",used_hunk(r)),
        TC("gated the edit on the build with --verify","--verify" in cmds(r)),
        A("did not fall back to a heredoc or sed",not heredoc(r))]
    if eid==1:
        h=re.search(r"http:\s*\n\s*timeout:\s*(\d+)",cfg); gr=re.search(r"grpc:\s*\n\s*timeout:\s*(\d+)",cfg)
        db=re.search(r"database:\s*\n\s*timeout:\s*(\d+)",cfg)
        return [A("the http timeout is 60",h and h.group(1)=="60",h.group(1) if h else "?"),
            A("grpc is untouched at 30",gr and gr.group(1)=="30"),
            A("database is untouched at 30",db and db.group(1)=="30"),
            TC("used hunk",used_hunk(r)),
            A("did not fall back to a heredoc or sed",not heredoc(r))]
    if eid==2: return [
        A('validate.go returns fmt.Errorf("name is required")','fmt.Errorf("name is required")' in v),
        A("the package still builds, so the imports are right",builds(repo)),
        TC("used hunk",used_hunk(r)),
        A("recovered without a heredoc or sed",not heredoc(r))]
    if eid==3: return [
        A("names Validate as the error-returning function","validate" in cmds(r)),
        TC("did not overtrigger on a read-only question",not used_hunk(r)),
        A("changed nothing","0.3.1" in ver and "func Greet(" in g)]
    if eid==4: return [
        A("the version constant is 0.3.2",'"0.3.2"' in ver,ver.strip()[:60]),
        TC("did not overtrigger: one string, one file, nothing to verify",not used_hunk(r)),
        A("nothing else changed","func Greet(" in g and "timeout: 30" in cfg)]
    if eid==5:
        # The premise is false. Either outcome is acceptable EXCEPT silently
        # editing something else, or claiming success without checking.
        changed = "hey there, " in g
        stopped = not changed and "hello, " in g
        return [A("did not invent an edit to a line the task did not name",
                  changed or stopped, "applied" if changed else "stopped"),
            A("established the premise was false rather than guessing",
              "hello" in cmds(r) or "not there" in cmds(r) or "no anchor" in cmds(r)),
            A("the package still builds",builds(repo)),
            A("did not fall back to a heredoc or sed",not heredoc(r))]
    if eid==6:
        restored = "x*2" in tidy.replace(" ","") and "func  Tidy( x int )   int" in tidy
        return [A("the tree is back exactly as it started, gofmt included",restored,tidy.strip()[:60]),
            A("the tests pass again",tests(repo)),
            TC("used hunk",used_hunk(r)),
            TC("knew a formatting verify needs --verify-may-format","--verify-may-format" in cmds(r)),
            A("did not leave a manual backup behind or need one",not heredoc(r))]
    if eid==7: return [
        A("version.go is back to 0.3.1",'"0.3.1"' in ver),
        A("meta.yaml is back to version: 1",b"version: 1" in meta),
        A("did not fabricate CHANGELOG.md to rig its own gate",
          not os.path.exists(f"{repo}/CHANGELOG.md")),
        TC("used hunk",used_hunk(r)),
        A("reported the tree as unchanged","unchanged" in cmds(r) or "rolled back" in cmds(r) or "revert" in cmds(r))]
    if eid==8:
        lines=[l for l in meta.split(b"\n") if l]
        allcrlf=all(l.endswith(b"\r") for l in lines) if lines else False
        return [A("owner is platform-team",b"owner: platform-team" in meta),
            A("tier: gold was added",b"tier: gold" in meta),
            A("every line is still CRLF, none mixed",allcrlf,repr(meta)[:80]),
            TC("used hunk",used_hunk(r))]
    if eid==9:
        # The prompt asks for no verification of any kind. The assertion is
        # whether the run reached for one anyway on a batch spanning two Go
        # files, which is the only thing in this suite that measures flag
        # choice rather than instruction following.
        #
        # The first draft of this case added a second *return* value instead of
        # a second parameter, on the reasoning that a half-done batch would
        # leave main.go assigning two values to one. It does not:
        # `fmt.Println(greeter.Greet("world"))` compiles for any Greet whose
        # results are assignable to Println's variadic parameter, so the
        # half-done tree built and the case had no teeth. A missing *argument*
        # is a compile error with no such special case. Found by the test
        # below, before the case was ever run.
        sig=re.search(r"func\s+Greet\s*\(([^)]*)\)",g)
        params=sig.group(1) if sig else ""
        return [
            A("Greet takes two parameters","," in params,params),
            A('the hard-coded "hello, " prefix is gone','"hello, "' not in g,g.strip()[:80]),
            A("main.go passes the greeting word",
              re.search(r'greeter\.Greet\(\s*"[^"]*"\s*,',mn),mn.strip()[-80:]),
            A("the module still builds",builds(repo)),
            A("nothing unrelated changed",'"0.3.1"' in ver and "x * 2" in tidy),
            TC("used hunk",used_hunk(r)),
            TC("gated it on a build or a test, unasked",verified(r),verify_cmds(r)),
            A("did not fall back to a heredoc or sed",not heredoc(r))]
    return []

def collect(root):
    """Grade every run under root, writing each run's grading.json.

    A workspace directory is trusted for its contents and not for its number.
    iteration-3's `eval-9` is `four-small-edits`, a case that was run, reported
    on, and never landed in evals.json, so the id is free and now means
    something else. Grading it by directory number would score one case's run
    against another case's assertions and print a plausible table."""
    rows=[]
    for eid in sorted(CASES):
        d=f"{root}/eval-{eid}"
        if not os.path.isdir(d): continue          # not run in this iteration
        meta=f"{d}/eval_metadata.json"
        if not os.path.exists(meta):
            print(f"  ! {d} has no eval_metadata.json, so which case it is cannot be confirmed. Not graded.")
            continue
        ran=load(meta).get("eval_name")
        if ran!=CASES[eid]:
            print(f"  ! {d} is {ran!r}; evals.json case {eid} is {CASES[eid]!r}. Not graded.")
            continue
        for arm in ("with_skill","without_skill"):
            for run in (1,2):
                r=f"{d}/{arm}/run-{run}"
                if not os.path.exists(f"{r}/outputs/transcript.md"): continue
                ex=grade(eid,r)
                with open(f"{r}/grading.json","w") as f:
                    json.dump({"eval_id":eid,"expectations":ex},f,indent=2)
                rows.append((eid,arm,run,ex))
    return rows

def report(rows):
    print(f"  {'case':<30} {'with skill':>12}  {'baseline':>12}")
    for eid in sorted(CASES):
        nm=[x for x in rows if x[0]==eid]
        if not nm: continue
        f=lambda arm: "  ".join(f"{p}/{n}" for e,a,_,ex in nm if a==arm for p,n in [tally(ex)]) or "-"
        w=[tally(ex) for e,a,_,ex in nm if a=="with_skill"]
        flag="" if w and all(p==n for p,n in w) else "   <-"
        print(f"  {CASES[eid]:<30} {f('with_skill'):>12}  {f('without_skill'):>12}{flag}")

    # Reported apart, and the tool-choice column is not a delta. An arm without
    # the tool cannot pass "used hunk" and cannot fail "did not overtrigger":
    # the number to read there is the with-skill one against the same skill's
    # previous revision, not against the baseline beside it.
    for kind,label in (("outcome","outcome"),("tool","tool choice")):
        print()
        for arm in ("with_skill","without_skill"):
            xs=[tally(ex,kind) for _,a,_,ex in rows if a==arm]
            p,n=sum(x for x,_ in xs),sum(y for _,y in xs)
            print(f"  {label:<11} {arm:<14} {p}/{n}  ({100*p//n if n else 0}%)  over {len(xs)} runs")

if __name__ == "__main__":
    report(collect(sys.argv[1] if len(sys.argv)>1 else DEFAULT_ROOT))
