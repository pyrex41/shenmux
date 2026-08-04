\\ Bifrost/Shen transition traces for the pure control-plane reducer.
(tc -)
(load "specs/mux.shen")

(define mux.assert
  {A --> A --> string --> boolean}
  X X _ -> true
  X Y Label -> (error (cn Label (cn ": expected equality, got "
                                  (cn (str X) (cn " / " (str Y)))))))

(define mux.accepted?
  {(list A) --> boolean}
  ["accepted" _ _] -> true
  _ -> false)

(define mux.rejected?
  {(list A) --> string --> boolean}
  ["rejected" Reason] Reason -> true
  _ _ -> false)

(define mux.trace-attach
  {number --> boolean}
  _ ->
    (let Dim [80 24]
      (let Snap [0 Dim 0 0 false ""]
        (let S0 [[] 0 Snap Dim false false []]
          (let Begin (mux.reduce S0 ["begin-attach" "client-1"])
            (let Finish (mux.reduce (head (tail Begin))
                                    ["finish-attach" "client-1" Snap])
              (let S1 (head (tail Finish))
                (and (mux.assert (mux.accepted? Begin) true "begin attach")
                     (and (mux.assert (mux.accepted? Finish) true "finish attach")
                          (and (mux.assert (mux.member? "client-1" (head S1))
                                             true "client attached")
                               (mux.assert (head (tail (tail (tail (tail S1)))))
                                           false "lock released")))))))))))

(define mux.base
  {number --> session}
  _ -> [["client-1"] 0 [0 [80 24] 0 0 false ""] [80 24] false false []])

(define mux.after-acquire
  {number --> session}
  N -> (head (tail (mux.reduce (mux.base N) ["acquire-control" "client-1"]))))

(define mux.after-resize
  {session --> session}
  S -> (head (tail (mux.reduce S ["resize" "client-1" [100 40]]))))

(define mux.after-output
  {session --> session}
  S -> (head (tail (mux.reduce S ["pty-output" 12]))))

(define mux.after-exit
  {session --> session}
  S -> (head (tail (mux.reduce S ["process-exit" 7]))))

(define mux.trace-control-and-events
  {number --> boolean}
  N ->
    (let S1 (mux.after-acquire N)
      (let S2 (mux.after-resize S1)
        (let S3 (mux.after-output S2)
          (let S4 (mux.after-exit S3)
            (and (mux.assert (head (tail S1)) 1 "control sequence")
                 (and (mux.assert (head (tail S2)) 2 "resize sequence")
                      (and (mux.assert (head (tail S3)) 3 "output sequence")
                           (mux.assert (head (tail S4)) 4 "exit sequence")))))))))

(define mux.trace-rejections
  {number --> boolean}
  _ ->
    (let S [["owner" "observer"] 0 [0 [80 24] 0 0 false ""] [80 24]
            false false ["owner"]]
      (and (mux.assert (mux.rejected? (mux.reduce S ["input" "observer" 1])
                                      "control-owned") true "observer rejected")
           (and (mux.assert (mux.rejected? (mux.reduce S ["input" "missing" 1])
                                           "not-attached") true "missing rejected")
                (mux.assert (mux.rejected? (mux.reduce S ["attach" "owner"])
                                        "already-attached") true "duplicate rejected")))))

(define mux.trace-barriers
  {number --> boolean}
  _ ->
    (let Dim [80 24]
      (let Snap [0 Dim 0 0 false ""]
        (let S0 [[] 0 Snap Dim false false []]
          (let Begin (mux.reduce S0 ["begin-attach" "client-1"])
            (let Locked (head (tail Begin))
              (and
                (mux.assert (mux.rejected? (mux.reduce Locked ["finish-attach" "client-2" Snap])
                                            "snapshot-owner-mismatch") true "attach owner mismatch")
                (and
                  (mux.assert (mux.rejected? (mux.reduce Locked ["finish-attach" "client-1"
                                                                  [1 Dim 0 0 false ""]])
                                              "snapshot-mismatch") true "attach sequence mismatch")
                  (and
                    (mux.assert (mux.rejected? (mux.reduce Locked ["begin-attach" "client-2"])
                                                "writer-locked") true "attach lock exclusion")
                    (mux.assert (mux.accepted? (mux.reduce Locked ["finish-attach" "client-1" Snap]))
                                true "attach completion"))))))))))

(define mux.trace-command-contract
  {number --> boolean}
  N ->
    (let Base (mux.base N)
      (let Acquired (mux.reduce Base ["acquire-control" "client-1"])
        (let Controlled (head (tail Acquired))
          (let Input (mux.reduce Controlled ["input" "client-1" 41])
            (let Resize (mux.reduce Controlled ["resize" "client-1" [100 40]])
              (let Released (mux.reduce Controlled ["release-control" "client-1"])
                (let Leased (mux.reduce Controlled ["lease-expired" "client-1"])
                  (let Detached (mux.reduce Controlled ["detach" "client-1"])
                    (let Exited (mux.reduce Controlled ["process-exit" 7])
                      (let ExitedState (head (tail Exited))
                        (and
                          (mux.assert (head (tail (tail Input)))
                                      [["write-pty" "client-1" 41]] "input effect")
                          (and
                            (mux.assert (head (tail (tail Resize)))
                                        [["resize-pty" [100 40]] ["publish" 2 "delta" 0]]
                                        "resize effects")
                            (and
                              (mux.assert (head (tail (head (tail Released)))) 2 "release sequence")
                              (and
                                (mux.assert (head (tail (head (tail Leased)))) 2 "lease sequence")
                                (and
                                  (mux.assert (mux.member? "client-1" (head (head (tail Detached))))
                                              false "detach membership")
                                  (and
                                    (mux.assert (mux.rejected? (mux.reduce Base ["resync" "missing"])
                                                               "not-attached") true "resync membership")
                                    (and
                                      (mux.assert (mux.accepted? (mux.reduce Base ["resync" "client-1"]))
                                                  true "resync attached")
                                      (and
                                        (mux.assert (mux.rejected? (mux.reduce ExitedState ["pty-output" 9])
                                                                   "exited") true "output after exit")
                                        (and
                                          (mux.assert (mux.rejected? (mux.reduce ExitedState ["process-exit" 8])
                                                                     "exited") true "repeat exit")
                                          (mux.assert (mux.rejected? (mux.reduce Base ["bogus"])
                                                                     "unknown-command") true
                                                      "unknown command")))))))))))))))))))))

(define mux.run-tests
  {number --> boolean}
  N -> (and (mux.trace-attach N)
            (and (mux.trace-control-and-events N)
                 (and (mux.trace-rejections N)
                      (and (mux.trace-barriers N)
                           (mux.trace-command-contract N))))))

(do (mux.run-tests 0)
    (do (print "ALL PASS") (nl)))
