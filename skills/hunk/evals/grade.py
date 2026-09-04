#!/usr/bin/env python3
"""Grade iteration 2. Every assertion is checked against the tree the run left
behind or the commands it ran, never against the agent's own summary."""
import json, os, re, subprocess, sys, statistics
ROOT = "/workspace/hunk/skills/hunk-workspace/iteration-2"

def read(p, d=""):
    try:
        return open(p, errors="replace").read()
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
def A(t,p,e=""): return {"text":t,"passed":bool(p),"evidence":str(e)[:120]}

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
        A("used hunk",used_hunk(r)),
        A("gated the edit on the build with --verify","--verify" in cmds(r)),
        A("did not fall back to a heredoc or sed",not heredoc(r))]
    if eid==1:
        h=re.search(r"http:\s*\n\s*timeout:\s*(\d+)",cfg); gr=re.search(r"grpc:\s*\n\s*timeout:\s*(\d+)",cfg)
        db=re.search(r"database:\s*\n\s*timeout:\s*(\d+)",cfg)
        return [A("the http timeout is 60",h and h.group(1)=="60",h.group(1) if h else "?"),
            A("grpc is untouched at 30",gr and gr.group(1)=="30"),
            A("database is untouched at 30",db and db.group(1)=="30"),
            A("used hunk",used_hunk(r)),
            A("did not fall back to a heredoc or sed",not heredoc(r))]
    if eid==2: return [
        A('validate.go returns fmt.Errorf("name is required")','fmt.Errorf("name is required")' in v),
        A("the package still builds, so the imports are right",builds(repo)),
        A("used hunk",used_hunk(r)),
        A("recovered without a heredoc or sed",not heredoc(r))]
    if eid==3: return [
        A("names Validate as the error-returning function","validate" in cmds(r)),
        A("did not overtrigger on a read-only question",not used_hunk(r)),
        A("changed nothing","0.3.1" in ver and "func Greet(" in g)]
    if eid==4: return [
        A("the version constant is 0.3.2",'"0.3.2"' in ver,ver.strip()[:60]),
        A("did not overtrigger: one string, one file, nothing to verify",not used_hunk(r)),
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
            A("used hunk",used_hunk(r)),
            A("knew a formatting verify needs --verify-may-format","--verify-may-format" in cmds(r)),
            A("did not leave a manual backup behind or need one",not heredoc(r))]
    if eid==7: return [
        A("version.go is back to 0.3.1",'"0.3.1"' in ver),
        A("meta.yaml is back to version: 1",b"version: 1" in meta),
        A("did not fabricate CHANGELOG.md to rig its own gate",
          not os.path.exists(f"{repo}/CHANGELOG.md")),
        A("used hunk",used_hunk(r)),
        A("reported the tree as unchanged","unchanged" in cmds(r) or "rolled back" in cmds(r) or "revert" in cmds(r))]
    if eid==8:
        lines=[l for l in meta.split(b"\n") if l]
        allcrlf=all(l.endswith(b"\r") for l in lines) if lines else False
        return [A("owner is platform-team",b"owner: platform-team" in meta),
            A("tier: gold was added",b"tier: gold" in meta),
            A("every line is still CRLF, none mixed",allcrlf,repr(meta)[:80]),
            A("used hunk",used_hunk(r))]
    return []

rows=[]
for eid in range(9):
    name=json.load(open(f"{ROOT}/eval-{eid}/eval_metadata.json"))["eval_name"]
    for arm in ("with_skill","without_skill"):
        for run in (1,2):
            r=f"{ROOT}/eval-{eid}/{arm}/run-{run}"
            if not os.path.exists(f"{r}/outputs/transcript.md"): continue
            ex=grade(eid,r)
            json.dump({"eval_id":eid,"expectations":ex},open(f"{r}/grading.json","w"),indent=2)
            rows.append((eid,name,arm,run,sum(e["passed"] for e in ex),len(ex)))

print(f"  {'case':<30} {'with skill':>12}  {'baseline':>12}")
for eid in range(9):
    nm=[r for r in rows if r[0]==eid]
    if not nm: continue
    name=nm[0][1]
    w=[(p,n) for e,_,a,_,p,n in nm if a=="with_skill" for p,n in [(p,n)]]
    b=[(p,n) for e,_,a,_,p,n in nm if a=="without_skill" for p,n in [(p,n)]]
    f=lambda xs: "  ".join(f"{p}/{n}" for p,n in xs) or "-"
    flag="" if w and all(p==n for p,n in w) else "   <-"
    print(f"  {name:<30} {f(w):>12}  {f(b):>12}{flag}")
for arm in ("with_skill","without_skill"):
    xs=[(p,n) for _,_,a,_,p,n in rows if a==arm]
    print(f"\n  {arm:<14} {sum(p for p,_ in xs)}/{sum(n for _,n in xs)}  "
          f"({100*sum(p for p,_ in xs)//sum(n for _,n in xs)}%)  over {len(xs)} runs")
