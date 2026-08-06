\\ specs/mux.shen
\\ Executable pure protocol authority for shenmux.
\\ The host owns PTY, ZeroMQ, terminal FFI, clocks, and persistence. These
\\ definitions own legal session transitions. Screen deltas, control changes,
\\ and exit share one monotonic sequence.

(datatype client-id
  Id : string;
  (not (empty? Id)) : verified;
  ==============================
  Id : client-id;)

(datatype seq-no
  N : number;
  (>= N 0) : verified;
  =====================
  N : seq-no;)

(datatype dimensions
  Cols : number;
  Rows : number;
  (> Cols 0) : verified;
  (> Rows 0) : verified;
  ================================
  [Cols Rows] : dimensions;)

\\ An instant is a host-supplied monotonic reading in milliseconds. The model
\\ never reads a clock; it is told what time it is and compares two numbers.
(datatype instant
  N : number;
  (>= N 0) : verified;
  ====================
  N : instant;)

\\ A clock is the pair the host must supply with every reduction: what time it
\\ is, and how long a control lease is believed without further evidence.
\\ Lease is positive by construction, so "the lease window is zero" -- the shape
\\ of a host that forgot to configure it, and which would otherwise make every
\\ owner instantly stale -- is not a representable clock.
(datatype clock
  Now : instant;
  Lease : number;
  (> Lease 0) : verified;
  =========================
  [Now Lease] : clock;)

\\ Snapshot payload is a bounded canonical screen checkpoint plus a bounded
\\ interpreted transition tail. It never contains raw PTY control strings.
(datatype snapshot
  Seq : seq-no;
  Dim : dimensions;
  CursorX : number;
  CursorY : number;
  AltScreen : boolean;
  Payload : string;
  (>= CursorX 0) : verified;
  (>= CursorY 0) : verified;
  ============================================================
  [Seq Dim CursorX CursorY AltScreen Payload] : snapshot;)

\\
\\ Constructor predicates are named functions so generated host constructors
\\ execute the same rules rather than re-encoding datatype premises.
(define mux.valid-client-id?
  {string --> boolean}
  Id -> (not (= Id "")))

(define mux.valid-dimensions?
  {number --> number --> boolean}
  Cols Rows -> (and (> Cols 0) (> Rows 0)))

(define mux.valid-clock?
  {number --> number --> boolean}
  Now Lease -> (and (>= Now 0) (> Lease 0)))

(define mux.valid-snapshot-meta?
  {number --> number --> number --> number --> number --> boolean}
  Seq Cols Rows CursorX CursorY ->
    (and (>= Seq 0)
         (and (mux.valid-dimensions? Cols Rows)
              (and (>= CursorX 0)
                   (and (< CursorX Cols)
                        (and (>= CursorY 0)
                             (< CursorY Rows)))))))

\\ [Clients LastSeq LastSnap Dim WriterLocked Exited [Controller PendingAttach]
\\  Heard]
\\ Controller is [] for no owner or [Cid] for exactly one owner. PendingAttach
\\ is [] normally or [Cid] while an attach snapshot is in flight.
\\
\\ Heard is when the current owner last EXERCISED control -- input, resize, or
\\ (re-)acquisition -- as an instant, or 0 when there is no owner. It is
\\ deliberately not "when we last heard from that client": a keepalive proves a
\\ process is running, and in shenmux the process holding a lease is routinely a
\\ relay proxy standing in for a browser that has closed. Ownership is therefore
\\ an assertion with an age, never a fact, and the age is what a competing claim
\\ is measured against.
(datatype session
  Clients : (list client-id);
  LastSeq : seq-no;
  LastSnap : snapshot;
  Dim : dimensions;
  WriterLocked : boolean;
  Exited : boolean;
  Control : (list (list client-id));
  Heard : instant;
  ==============================================================================
  [Clients LastSeq LastSnap Dim WriterLocked Exited Control Heard] : session;)

(define mux.member?
  {A --> (list A) --> boolean}
  _ [] -> false
  X [X | _] -> true
  X [_ | Rest] -> (mux.member? X Rest))

(define mux.controller
  {boolean --> (list (list client-id)) --> (list client-id)}
  _ [Owner _] -> Owner)

(define mux.pending
  {boolean --> (list (list client-id)) --> (list client-id)}
  false _ -> []
  true [_ Pending] -> Pending)

(define mux.lock-control
  {(list (list client-id)) --> client-id --> (list (list client-id))}
  [Owner _] Pending -> [Owner [Pending]])

(define mux.remove-client
  {client-id --> (list client-id) --> (list client-id)}
  _ [] -> []
  C [C | Rest] -> Rest
  C [X | Rest] -> [X | (mux.remove-client C Rest)])

(define mux.release-if-owner
  {client-id --> (list client-id) --> (list client-id)}
  C [C] -> []
  _ Owner -> Owner)

\\ Releasing ownership must clear the ownership evidence with it, so a later
\\ acquisition can never inherit a previous owner's freshness.
(define mux.heard-on-release
  {client-id --> (list client-id) --> number --> number}
  C [C] _ -> 0
  _ _ Heard -> Heard)

(define mux.clock-now
  {clock --> number}
  [Now _] -> Now)

(define mux.clock-lease
  {clock --> number}
  [_ Lease] -> Lease)

\\ Freshness is a comparison of two numbers the host supplied, expressed with
\\ addition rather than subtraction because the evaluator's arithmetic is over
\\ nonnegative integers and Heard may legitimately exceed Now on a host whose
\\ readings are not perfectly ordered.
(define mux.owner-fresh?
  {clock --> number --> boolean}
  Clock Heard -> (<= (mux.clock-now Clock) (+ Heard (mux.clock-lease Clock))))

(define mux.session-heard
  {session --> number}
  [_ _ _ _ _ _ _ Heard] -> Heard)

\\ Record that the owner exercised control at Now. This is the ONLY producer of
\\ ownership evidence; nothing that merely proves a peer's process is alive may
\\ call it.
(define mux.mark-heard
  {session --> clock --> session}
  [Clients Seq Snap Dim Locked Exited Control _] Clock ->
    [Clients Seq Snap Dim Locked Exited Control (mux.clock-now Clock)])

(define mux.attach-ok?
  {session --> client-id --> boolean}
  [Clients _ _ _ Locked _ Control _] C ->
    (and (not Locked)
         (not (mux.member? C Clients))))

(define mux.has-control?
  {session --> client-id --> boolean}
  [_ _ _ _ Locked _ Control _] C ->
    (= (mux.controller Locked Control) [C]))

(define mux.accept-input?
  {session --> client-id --> boolean}
  [Clients _ _ _ Locked Exited Control _] C ->
    (and (not Locked)
         (and (not Exited)
              (and (mux.member? C Clients)
                   (= (mux.controller Locked Control) [C])))))

(define mux.accept-resize?
  {session --> client-id --> boolean}
  S C -> (mux.accept-input? S C))

\\ Takes a clock because an owner whose evidence has aged past the lease no
\\ longer excludes a competing claim. See mux.reason-acquire.
(define mux.acquire-control-ok?
  {session --> clock --> client-id --> boolean}
  [Clients _ _ _ Locked Exited Control Heard] Clock C ->
    (and (not Locked)
         (and (not Exited)
              (and (mux.member? C Clients)
                   (or (= (mux.controller Locked Control) [])
                       (or (= (mux.controller Locked Control) [C])
                           (not (mux.owner-fresh? Clock Heard))))))))

(define mux.release-control-ok?
  {session --> client-id --> boolean}
  S C -> (mux.has-control? S C))

(define mux.begin-snapshot
  {session --> session}
  [Clients Seq Snap Dim _ Exited [Owner _] Heard] ->
    [Clients Seq Snap Dim true Exited [Owner []] Heard])

(define mux.end-snapshot
  {session --> snapshot --> session}
  [Clients Seq _ Dim _ Exited [Owner _] Heard] NewSnap ->
    [Clients Seq NewSnap Dim false Exited [Owner []] Heard])

(define mux.attach
  {session --> client-id --> session}
  [Clients Seq Snap Dim Locked Exited Control Heard] C ->
    [[C | Clients] Seq Snap Dim Locked Exited Control Heard])

(define mux.detach
  {session --> client-id --> session}
  [Clients Seq Snap Dim Locked Exited Control Heard] C ->
    [(mux.remove-client C Clients)
     Seq Snap Dim Locked Exited [(mux.release-if-owner C (mux.controller Locked Control)) []]
     (mux.heard-on-release C (mux.controller Locked Control) Heard)])

\\ Acquisition is itself evidence of control, so it stamps Heard. A client that
\\ takes control from a stale owner therefore starts its own lease at Now rather
\\ than inheriting an expired one.
(define mux.acquire-control
  {session --> clock --> client-id --> session}
  [Clients Seq Snap Dim Locked Exited Control Heard] Clock C ->
    [Clients Seq Snap Dim Locked Exited [[C] []] (mux.clock-now Clock)])

(define mux.release-control
  {session --> client-id --> session}
  [Clients Seq Snap Dim Locked Exited Control Heard] C ->
    [Clients Seq Snap Dim Locked Exited [(mux.release-if-owner C (mux.controller Locked Control)) []]
     (mux.heard-on-release C (mux.controller Locked Control) Heard)])

(define mux.next-seq
  {seq-no --> seq-no}
  N -> (+ N 1))

(define mux.event-ok?
  {session --> seq-no --> boolean}
  [_ Current _ _ Locked _ Control _] Candidate ->
    (and (not Locked)
         (= Candidate (mux.next-seq Current))))

(define mux.apply-delta
  {session --> seq-no --> session}
  [Clients _ Snap Dim Locked Exited Control Heard] Seq ->
    [Clients Seq Snap Dim Locked Exited Control Heard])

(define mux.apply-resize
  {session --> seq-no --> dimensions --> session}
  [Clients _ Snap _ Locked Exited Control Heard] Seq Dim ->
    [Clients Seq Snap Dim Locked Exited Control Heard])

(define mux.apply-control
  {session --> seq-no --> session}
  [Clients _ Snap Dim Locked Exited Control Heard] Seq ->
    [Clients Seq Snap Dim Locked Exited Control Heard])

(define mux.apply-exit
  {session --> seq-no --> session}
  [Clients _ Snap Dim Locked _ Control Heard] Seq ->
    [Clients Seq Snap Dim Locked true Control Heard])

\\ -------------------------------------------------------------------------
\\ Control-plane reducer
\\
\\ Commands and effects are deliberately plain tagged lists.  The host keeps
\\ payloads opaque: Shen decides whether a payload may cross an edge, but
\\ never parses terminal bytes or snapshot archives.
\\
\\ Command forms:
\\   ["begin-attach" C] ["finish-attach" C Snap] ["detach" C]
\\   ["acquire-control" C] ["release-control" C]
\\   ["input" C Payload] ["resize" C Dim]
\\   ["begin-snapshot"] ["finish-snapshot" Snap]
\\   ["pty-output" Payload] ["process-exit" Code]
\\   ["lease-expired" C] ["resync" C] ["peer-lost" C]
\\
\\ Every reduction carries a clock. The model does not read one; the host says
\\ what time it is, and the model decides what that makes true.
\\
\\ Result forms:
\\   ["accepted" NewState Effects] | ["rejected" Reason]
\\
\\ Effects are descriptions for Go adapters, never host operations.

(define mux.accepted
  {session --> (list string) --> (list string)}
  S Effects -> ["accepted" S Effects])

(define mux.rejected
  {string --> (list string)}
  Reason -> ["rejected" Reason])

(define mux.reason-attach
  {session --> client-id --> string}
  [Clients _ _ _ Locked _ Control _] C ->
    (if Locked "writer-locked"
        (if (mux.member? C Clients) "already-attached" "ok")))

(define mux.reason-member
  {session --> client-id --> string}
  [Clients _ _ _ _ _ Control _] C ->
    (if (mux.member? C Clients) "ok" "not-attached"))

\\ Deliberately clock-blind. A quiet owner is not evicted from its own session:
\\ typing after a long silence is accepted and re-proves the claim. Staleness is
\\ visible only to a COMPETING claim (mux.reason-acquire), which is what a lease
\\ is for and is the whole of the difference from eviction.
(define mux.reason-input
  {session --> client-id --> string}
  [Clients _ _ _ Locked Exited Control _] C ->
    (if (not (mux.member? C Clients)) "not-attached"
        (if Exited "exited"
            (if Locked "writer-locked"
                    (if (= (mux.controller Locked Control) []) "no-control"
                    (if (= (mux.controller Locked Control) [C]) "ok" "control-owned"))))))

\\ The preemption rule. "control-owned" is only returned against an owner whose
\\ evidence is still inside the lease window; an owner that has not exercised
\\ control since then does not exclude anyone.
\\
\\ This is the rule that keeps a session typeable when a control lease is parked
\\ on a client id nobody can reach: the parked lease stops being an answer once
\\ it is old, without anything having to notice the peer left.
(define mux.reason-acquire
  {session --> clock --> client-id --> string}
  [Clients _ _ _ Locked Exited Control Heard] Clock C ->
    (if (not (mux.member? C Clients)) "not-attached"
        (if Exited "exited"
            (if Locked "writer-locked"
                (if (= (mux.controller Locked Control) []) "ok"
                    (if (= (mux.controller Locked Control) [C]) "ok"
                        (if (mux.owner-fresh? Clock Heard) "control-owned" "ok")))))))

(define mux.snapshot-matches?
  {session --> snapshot --> boolean}
  [_ Seq Snap Dim _ _ Control _] NewSnap ->
    (and (= (head NewSnap) Seq)
         (= (head (tail NewSnap)) Dim)))

(define mux.dimensions-valid-value?
  {dimensions --> boolean}
  [Cols Rows] -> (mux.valid-dimensions? Cols Rows))

(define mux.pending-client?
  {session --> client-id --> boolean}
  [_ _ _ _ Locked _ Control _] C -> (= (mux.pending Locked Control) [C]))

(define mux.begin-attach
  {session --> client-id --> (list A)}
  [Clients Seq Snap Dim Locked Exited Control Heard] C ->
    (if Locked (mux.rejected "writer-locked")
        (mux.accepted [Clients Seq Snap Dim true Exited (mux.lock-control Control C) Heard]
                      [["capture-snapshot"]])))

(define mux.complete-attach
  {session --> client-id --> snapshot --> (list A)}
  [Clients Seq OldSnap Dim Locked Exited Control Heard] C NewSnap ->
    (if (not Locked) (mux.rejected "writer-unlocked")
        (if (not (= (mux.pending Locked Control) [C])) (mux.rejected "snapshot-owner-mismatch")
            (if (not (mux.snapshot-matches? [Clients Seq OldSnap Dim Locked Exited Control Heard] NewSnap))
                (mux.rejected "snapshot-mismatch")
                (mux.accepted [(if (mux.member? C Clients) Clients [C | Clients])
                               Seq NewSnap Dim false Exited [(mux.controller Locked Control) []] Heard]
                              [["reply" C 0]])))))

(define mux.reduce-attach
  {session --> client-id --> (list A)}
  S C ->
    (if (= (mux.reason-attach S C) "ok")
        (mux.accepted (mux.attach S C) [["reply" C 0]])
        (mux.rejected (mux.reason-attach S C))))

(define mux.reduce-begin-attach
  {session --> client-id --> (list A)}
  S C -> (mux.begin-attach S C))

(define mux.reduce-finish-attach
  {session --> client-id --> snapshot --> (list A)}
  S C Snap -> (mux.complete-attach S C Snap))

(define mux.reduce-detach
  {session --> client-id --> (list A)}
  S C ->
    (if (= (mux.reason-member S C) "ok")
        (let Next (mux.detach S C)
          (if (= (mux.has-control? S C) true)
              (let Seq (mux.next-seq (head (tail Next)))
                (mux.accepted (mux.apply-control Next Seq)
                              [["publish" Seq "control" 0]]))
              (mux.accepted Next [])))
        (mux.rejected "not-attached")))

(define mux.reduce-acquire
  {session --> clock --> client-id --> (list A)}
  S Clock C ->
    (if (= (mux.reason-acquire S Clock C) "ok")
          (if (mux.has-control? S C)
              (mux.accepted (mux.mark-heard S Clock) [])
              (let Next (mux.acquire-control S Clock C)
                (let Seq (mux.next-seq (head (tail Next)))
                  (mux.accepted (mux.apply-control Next Seq)
                                [["publish" Seq "control" 0]]))))
          (mux.rejected (mux.reason-acquire S Clock C))))

(define mux.reduce-release
  {session --> client-id --> (list A)}
  S C ->
    (if (mux.has-control? S C)
        (let Next (mux.release-control S C)
          (let Seq (mux.next-seq (head (tail Next)))
            (mux.accepted (mux.apply-control Next Seq)
                          [["publish" Seq "control" 0]])) )
        (if (= (mux.reason-member S C) "not-attached")
            (mux.rejected "not-attached")
            (mux.rejected "no-control"))))

(define mux.reduce-input
  {session --> clock --> client-id --> number --> (list A)}
  S Clock C Payload ->
    (if (= (mux.reason-input S C) "ok")
        (mux.accepted (mux.mark-heard S Clock) [["write-pty" C Payload]])
        (mux.rejected (mux.reason-input S C))))

(define mux.reduce-resize
  {session --> clock --> client-id --> dimensions --> (list A)}
  S Clock C NewDim ->
    (if (not (mux.dimensions-valid-value? NewDim))
        (mux.rejected "invalid-dimensions")
        (if (= (mux.reason-input S C) "ok")
            (if (= (head (tail (tail (tail S)))) NewDim)
            (mux.accepted (mux.mark-heard S Clock) [])
            (let Seq (mux.next-seq (head (tail S)))
              (mux.accepted (mux.mark-heard (mux.apply-resize S Seq NewDim) Clock)
                             [["resize-pty" NewDim]
                             ["publish" Seq "delta" 0]])))
            (mux.rejected (mux.reason-input S C)))))

(define mux.reduce-begin-snapshot
  {session --> (list A)}
  S ->
    (if (head (tail (tail (tail (tail S)))))
        (mux.rejected "writer-locked")
        (mux.accepted (mux.begin-snapshot S) [["capture-snapshot"]])))

(define mux.reduce-finish-snapshot
  {session --> snapshot --> (list A)}
  S Snap ->
    (if (not (head (tail (tail (tail (tail S))))))
        (mux.rejected "writer-unlocked")
        (if (mux.snapshot-matches? S Snap)
        (mux.accepted (mux.end-snapshot S Snap) [] )
            (mux.rejected "snapshot-mismatch"))))

(define mux.reduce-pty-output
  {session --> number --> (list A)}
  S Payload ->
    (if (head (tail (tail (tail (tail S)))))
        (mux.rejected "writer-locked")
        (if (head (tail (tail (tail (tail (tail S))))))
            (mux.rejected "exited")
            (let Seq (mux.next-seq (head (tail S)))
              (mux.accepted (mux.apply-delta S Seq)
                            [["publish" Seq "delta" Payload]])))))

(define mux.reduce-process-exit
  {session --> number --> (list A)}
  S Code ->
    (if (head (tail (tail (tail (tail S)))))
        (mux.rejected "writer-locked")
        (if (head (tail (tail (tail (tail (tail S))))))
            (mux.rejected "exited")
            (let Seq (mux.next-seq (head (tail S)))
              (mux.accepted (mux.apply-exit S Seq)
                            [["publish" Seq "exit" Code]])))))

(define mux.reduce-lease-expired
  {session --> client-id --> (list A)}
  S C ->
    (if (mux.has-control? S C)
        (let Next (mux.release-control S C)
          (let Seq (mux.next-seq (head (tail Next)))
            (mux.accepted (mux.apply-control Next Seq)
                          [["publish" Seq "control" 0]])) )
        (mux.accepted S [])))

(define mux.reduce-resync
  {session --> client-id --> (list A)}
  S C ->
    (if (= (mux.reason-member S C) "ok")
        (mux.accepted S [])
        (mux.rejected "not-attached")))

\\ -------------------------------------------------------------------------
\\ Delivery
\\
\\ mux.delivery-fatal? answers one question: a reply addressed to ONE named
\\ client could not be delivered -- does that end the session for everybody?
\\
\\ Almost never, and the shape of the rule matters more than the arms. The
\\ function is total and defaults to "no", because the outcomes it is asked
\\ about all arise at a send that already knows which client it was for, and
\\ nothing learned at such a send licenses ending anyone else's shell. Known
\\ per-peer outcomes are no-route, no-identity, not-draining (a peer that stopped
\\ taking delivery), and encode-failed -- none of them enumerated here, because
\\ enumerating them is what produced two rounds of "we missed one".
\\
\\ The socket's own death is not diagnosed here. It is observed on the receive
\\ path, which classifies independently; a send has no need to conclude it, and
\\ a send that concludes it wrongly costs everyone their session.

(define mux.delivery-fatal?
  {string --> boolean}
  "socket-closed" -> true
  _ -> false)

\\ Teardown after a per-peer delivery failure. Idempotent by construction: it
\\ never rejects, so every failure path can call it without first working out
\\ whether the client got as far as attaching. That decision used to live in the
\\ host, where it had to be got right once per call site.
(define mux.reduce-peer-lost
  {session --> client-id --> (list A)}
  S C ->
    (if (= (mux.reason-member S C) "ok")
        (mux.reduce-detach S C)
        (mux.accepted S [])))

(define mux.reduce
  {session --> clock --> (list A) --> (list A)}
  S _ ["begin-attach" C] -> (mux.reduce-begin-attach S C)
  S _ ["finish-attach" C Snap] -> (mux.reduce-finish-attach S C Snap)
  \\ Legacy/internal attach; network attach must use the barrier pair above.
  S _ ["attach" C] -> (mux.reduce-attach S C)
  S _ ["detach" C] -> (mux.reduce-detach S C)
  S _ ["peer-lost" C] -> (mux.reduce-peer-lost S C)
  S Clock ["acquire-control" C] -> (mux.reduce-acquire S Clock C)
  S _ ["release-control" C] -> (mux.reduce-release S C)
  S Clock ["input" C Payload] -> (mux.reduce-input S Clock C Payload)
  S Clock ["resize" C Dim] -> (mux.reduce-resize S Clock C Dim)
  S _ ["begin-snapshot"] -> (mux.reduce-begin-snapshot S)
  S _ ["finish-snapshot" Snap] -> (mux.reduce-finish-snapshot S Snap)
  S _ ["pty-output" Payload] -> (mux.reduce-pty-output S Payload)
  S _ ["process-exit" Code] -> (mux.reduce-process-exit S Code)
  S _ ["lease-expired" C] -> (mux.reduce-lease-expired S C)
  S _ ["resync" C] -> (mux.reduce-resync S C)
  _ _ _ -> ["rejected" "unknown-command"])
