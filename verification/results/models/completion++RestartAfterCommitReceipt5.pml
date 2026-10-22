// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go#L5
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
	chan child_RestartAfterCommitReceipt50 = [1] of {int}->
	run RestartAfterCommitReceipt5(child_RestartAfterCommitReceipt50)->
	child_RestartAfterCommitReceipt50?0->
stop_process:skip
}

proctype RestartAfterCommitReceipt5(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousRestartAfterCommitReceipt1162 = [1] of {int}->
	chan child_AnonymousRestartAfterCommitReceipt1061 = [1] of {int}->
	chan child_AnonymousRestartAfterCommitReceipt960 = [1] of {int}->
	Chandef executionHead_ch->
	Chandef originalBase_ch->
	Chandef receipt_ch->
	run sync_monitor(receipt_ch)->
	run sync_monitor(originalBase_ch)->
	run sync_monitor(executionHead_ch)->
	run AnonymousRestartAfterCommitReceipt96(receipt_ch,originalBase_ch,executionHead_ch,child_AnonymousRestartAfterCommitReceipt960)->
	run receiver(child_AnonymousRestartAfterCommitReceipt960)->
	run AnonymousRestartAfterCommitReceipt106(receipt_ch,originalBase_ch,executionHead_ch,child_AnonymousRestartAfterCommitReceipt1061)->
	run receiver(child_AnonymousRestartAfterCommitReceipt1061)->
	run AnonymousRestartAfterCommitReceipt116(receipt_ch,originalBase_ch,executionHead_ch,child_AnonymousRestartAfterCommitReceipt1162)->
	run receiver(child_AnonymousRestartAfterCommitReceipt1162)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:12:2 */
	:: executionHead_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:12:2 */->
	:: executionHead_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:12:2 */ -> 
		executionHead_ch.rcving!false
	fi->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:13:2 */
	:: originalBase_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:13:2 */->
	:: originalBase_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:13:2 */ -> 
		originalBase_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRestartAfterCommitReceipt96(Chandef receipt_ch;Chandef originalBase_ch;Chandef executionHead_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:9:14 */
	:: originalBase_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:9:14 */
	:: originalBase_ch.sync!false -> 
		originalBase_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:9:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRestartAfterCommitReceipt106(Chandef receipt_ch;Chandef originalBase_ch;Chandef executionHead_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:10:14 */
	:: receipt_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:10:14 */
	:: receipt_ch.sync!false -> 
		receipt_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:10:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousRestartAfterCommitReceipt116(Chandef receipt_ch;Chandef originalBase_ch;Chandef executionHead_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if
	:: receipt_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:11:19 */->
	:: receipt_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:11:19 */ -> 
		receipt_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:11:30 */
	:: executionHead_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:11:30 */
	:: executionHead_ch.sync!false -> 
		executionHead_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/restart.go:11:30 */
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

