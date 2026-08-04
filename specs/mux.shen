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

(define mux.valid-snapshot-meta?
  {number --> number --> number --> number --> number --> boolean}
  Seq Cols Rows CursorX CursorY ->
    (and (>= Seq 0)
         (and (mux.valid-dimensions? Cols Rows)
              (and (>= CursorX 0)
                   (and (< CursorX Cols)
                        (and (>= CursorY 0)
                             (< CursorY Rows)))))))

\\ [Clients LastSeq LastSnap Dim WriterLocked Exited [Controller PendingAttach]]
\\ Controller is [] for no owner or [Cid] for exactly one owner. PendingAttach
\\ is [] normally or [Cid] while an attach snapshot is in flight.
(datatype session
  Clients : (list client-id);
  LastSeq : seq-no;
  LastSnap : snapshot;
  Dim : dimensions;
  WriterLocked : boolean;
  Exited : boolean;
  Control : (list (list client-id));
  ==============================================================================
  [Clients LastSeq LastSnap Dim WriterLocked Exited Control] : session;)

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

(define mux.attach-ok?
  {session --> client-id --> boolean}
  [Clients _ _ _ Locked _ Control] C ->
    (and (not Locked)
         (not (mux.member? C Clients))))

(define mux.has-control?
  {session --> client-id --> boolean}
  [_ _ _ _ Locked _ Control] C ->
    (= (mux.controller Locked Control) [C]))

(define mux.accept-input?
  {session --> client-id --> boolean}
  [Clients _ _ _ Locked Exited Control] C ->
    (and (not Locked)
         (and (not Exited)
              (and (mux.member? C Clients)
                   (= (mux.controller Locked Control) [C])))))

(define mux.accept-resize?
  {session --> client-id --> boolean}
  S C -> (mux.accept-input? S C))

(define mux.acquire-control-ok?
  {session --> client-id --> boolean}
  [Clients _ _ _ Locked Exited Control] C ->
    (and (not Locked)
         (and (not Exited)
              (and (mux.member? C Clients)
                   (or (= (mux.controller Locked Control) [])
                       (= (mux.controller Locked Control) [C]))))))

(define mux.release-control-ok?
  {session --> client-id --> boolean}
  S C -> (mux.has-control? S C))

(define mux.begin-snapshot
  {session --> session}
  [Clients Seq Snap Dim _ Exited [Owner _]] ->
    [Clients Seq Snap Dim true Exited [Owner []]])

(define mux.end-snapshot
  {session --> snapshot --> session}
  [Clients Seq _ Dim _ Exited [Owner _]] NewSnap ->
    [Clients Seq NewSnap Dim false Exited [Owner []]])

(define mux.attach
  {session --> client-id --> session}
  [Clients Seq Snap Dim Locked Exited Control] C ->
    [[C | Clients] Seq Snap Dim Locked Exited Control])

(define mux.detach
  {session --> client-id --> session}
  [Clients Seq Snap Dim Locked Exited Control] C ->
    [(mux.remove-client C Clients)
     Seq Snap Dim Locked Exited [(mux.release-if-owner C (mux.controller Locked Control)) []]])

(define mux.acquire-control
  {session --> client-id --> session}
  [Clients Seq Snap Dim Locked Exited Control] C ->
    [Clients Seq Snap Dim Locked Exited [[C] []]])

(define mux.release-control
  {session --> client-id --> session}
  [Clients Seq Snap Dim Locked Exited Control] C ->
    [Clients Seq Snap Dim Locked Exited [(mux.release-if-owner C (mux.controller Locked Control)) []]])

(define mux.next-seq
  {seq-no --> seq-no}
  N -> (+ N 1))

(define mux.event-ok?
  {session --> seq-no --> boolean}
  [_ Current _ _ Locked _ Control] Candidate ->
    (and (not Locked)
         (= Candidate (mux.next-seq Current))))

(define mux.apply-delta
  {session --> seq-no --> session}
  [Clients _ Snap Dim Locked Exited Control] Seq ->
    [Clients Seq Snap Dim Locked Exited Control])

(define mux.apply-resize
  {session --> seq-no --> dimensions --> session}
  [Clients _ Snap _ Locked Exited Control] Seq Dim ->
    [Clients Seq Snap Dim Locked Exited Control])

(define mux.apply-control
  {session --> seq-no --> session}
  [Clients _ Snap Dim Locked Exited Control] Seq ->
    [Clients Seq Snap Dim Locked Exited Control])

(define mux.apply-exit
  {session --> seq-no --> session}
  [Clients _ Snap Dim Locked _ Control] Seq ->
    [Clients Seq Snap Dim Locked true Control])

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
\\   ["lease-expired" C] ["resync" C]
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
  [Clients _ _ _ Locked _ Control] C ->
    (if Locked "writer-locked"
        (if (mux.member? C Clients) "already-attached" "ok")))

(define mux.reason-member
  {session --> client-id --> string}
  [Clients _ _ _ _ _ Control] C ->
    (if (mux.member? C Clients) "ok" "not-attached"))

(define mux.reason-input
  {session --> client-id --> string}
  [Clients _ _ _ Locked Exited Control] C ->
    (if (not (mux.member? C Clients)) "not-attached"
        (if Exited "exited"
            (if Locked "writer-locked"
                    (if (= (mux.controller Locked Control) []) "no-control"
                    (if (= (mux.controller Locked Control) [C]) "ok" "control-owned"))))))

(define mux.reason-acquire
  {session --> client-id --> string}
  [Clients _ _ _ Locked Exited Control] C ->
    (if (not (mux.member? C Clients)) "not-attached"
        (if Exited "exited"
            (if Locked "writer-locked"
                (if (= (mux.controller Locked Control) []) "ok"
                    (if (= (mux.controller Locked Control) [C]) "ok" "control-owned"))))))

(define mux.snapshot-matches?
  {session --> snapshot --> boolean}
  [_ Seq Snap Dim _ _ Control] NewSnap ->
    (and (= (head NewSnap) Seq)
         (= (head (tail NewSnap)) Dim)))

(define mux.dimensions-valid-value?
  {dimensions --> boolean}
  [Cols Rows] -> (mux.valid-dimensions? Cols Rows))

(define mux.pending-client?
  {session --> client-id --> boolean}
  [_ _ _ _ Locked _ Control] C -> (= (mux.pending Locked Control) [C]))

(define mux.begin-attach
  {session --> client-id --> (list A)}
  [Clients Seq Snap Dim Locked Exited Control] C ->
    (if Locked (mux.rejected "writer-locked")
        (mux.accepted [Clients Seq Snap Dim true Exited (mux.lock-control Control C)]
                      [["capture-snapshot"]])))

(define mux.complete-attach
  {session --> client-id --> snapshot --> (list A)}
  [Clients Seq OldSnap Dim Locked Exited Control] C NewSnap ->
    (if (not Locked) (mux.rejected "writer-unlocked")
        (if (not (= (mux.pending Locked Control) [C])) (mux.rejected "snapshot-owner-mismatch")
            (if (not (mux.snapshot-matches? [Clients Seq OldSnap Dim Locked Exited Control] NewSnap))
                (mux.rejected "snapshot-mismatch")
                (mux.accepted [(if (mux.member? C Clients) Clients [C | Clients])
                               Seq NewSnap Dim false Exited [(mux.controller Locked Control) []]]
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
  {session --> client-id --> (list A)}
  S C ->
    (if (= (mux.reason-acquire S C) "ok")
          (if (mux.has-control? S C)
              (mux.accepted S [])
              (let Next (mux.acquire-control S C)
                (let Seq (mux.next-seq (head (tail Next)))
                  (mux.accepted (mux.apply-control Next Seq)
                                [["publish" Seq "control" 0]]))))
          (mux.rejected (mux.reason-acquire S C))))

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
  {session --> client-id --> number --> (list A)}
  S C Payload ->
    (if (= (mux.reason-input S C) "ok")
        (mux.accepted S [["write-pty" C Payload]])
        (mux.rejected (mux.reason-input S C))))

(define mux.reduce-resize
  {session --> client-id --> dimensions --> (list A)}
  S C NewDim ->
    (if (not (mux.dimensions-valid-value? NewDim))
        (mux.rejected "invalid-dimensions")
        (if (= (mux.reason-input S C) "ok")
            (if (= (head (tail (tail (tail S)))) NewDim)
            (mux.accepted S [])
            (let Seq (mux.next-seq (head (tail S)))
              (mux.accepted (mux.apply-resize S Seq NewDim)
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

(define mux.reduce
  {session --> (list A) --> (list A)}
  S ["begin-attach" C] -> (mux.reduce-begin-attach S C)
  S ["finish-attach" C Snap] -> (mux.reduce-finish-attach S C Snap)
  \\ Legacy/internal attach; network attach must use the barrier pair above.
  S ["attach" C] -> (mux.reduce-attach S C)
  S ["detach" C] -> (mux.reduce-detach S C)
  S ["acquire-control" C] -> (mux.reduce-acquire S C)
  S ["release-control" C] -> (mux.reduce-release S C)
  S ["input" C Payload] -> (mux.reduce-input S C Payload)
  S ["resize" C Dim] -> (mux.reduce-resize S C Dim)
  S ["begin-snapshot"] -> (mux.reduce-begin-snapshot S)
  S ["finish-snapshot" Snap] -> (mux.reduce-finish-snapshot S Snap)
  S ["pty-output" Payload] -> (mux.reduce-pty-output S Payload)
  S ["process-exit" Code] -> (mux.reduce-process-exit S Code)
  S ["lease-expired" C] -> (mux.reduce-lease-expired S C)
  S ["resync" C] -> (mux.reduce-resync S C)
  _ _ -> ["rejected" "unknown-command"])
