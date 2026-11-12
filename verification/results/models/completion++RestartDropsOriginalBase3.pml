// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go#L3
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_RestartDropsOriginalBase30 = [1] of {int}->
	run RestartDropsOriginalBase3(child_RestartDropsOriginalBase30)->
	child_RestartDropsOriginalBase30?0->
stop_process:skip
}

proctype RestartDropsOriginalBase3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousRestartDropsOriginalBase942 = [1] of {int}->
	chan child_AnonymousRestartDropsOriginalBase841 = [1] of {int}->
	chan child_AnonymousRestartDropsOriginalBase740 = [1] of {int}->
	Chandef executionHead_ch->
	Chandef originalBase_ch->
	Chandef receipt_ch->
	run sync_monitor(receipt_ch)->
	run sync_monitor(originalBase_ch)->
	run sync_monitor(executionHead_ch)->
	run AnonymousRestartDropsOriginalBase74(receipt_ch,originalBase_ch,executionHead_ch,child_AnonymousRestartDropsOriginalBase740)->
	run receiver(child_AnonymousRestartDropsOriginalBase740)->
	run AnonymousRestartDropsOriginalBase84(receipt_ch,originalBase_ch,executionHead_ch,child_AnonymousRestartDropsOriginalBase841)->
	run receiver(child_AnonymousRestartDropsOriginalBase841)->
	run AnonymousRestartDropsOriginalBase94(receipt_ch,originalBase_ch,executionHead_ch,child_AnonymousRestartDropsOriginalBase942)->
	run receiver(child_AnonymousRestartDropsOriginalBase942)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:10:2 */
	:: executionHead_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:10:2 */->
	:: executionHead_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:10:2 */ -> 
		executionHead_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRestartDropsOriginalBase74(Chandef receipt_ch;Chandef originalBase_ch;Chandef executionHead_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:7:13 */
	:: originalBase_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:7:13 */
	:: originalBase_ch.sync!false -> 
		originalBase_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:7:13 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRestartDropsOriginalBase84(Chandef receipt_ch;Chandef originalBase_ch;Chandef executionHead_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:8:13 */
	:: receipt_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:8:13 */
	:: receipt_ch.sync!false -> 
		receipt_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:8:13 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRestartDropsOriginalBase94(Chandef receipt_ch;Chandef originalBase_ch;Chandef executionHead_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if
	:: receipt_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:9:16 */->
	:: receipt_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:9:16 */ -> 
		receipt_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:9:27 */
	:: executionHead_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:9:27 */
	:: executionHead_ch.sync!false -> 
		executionHead_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/restart.go:9:27 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

