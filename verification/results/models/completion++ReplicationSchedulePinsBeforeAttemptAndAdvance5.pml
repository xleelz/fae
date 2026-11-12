// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go#L5
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
	chan child_ReplicationSchedulePinsBeforeAttemptAndAdvance50 = [1] of {int}->
	run ReplicationSchedulePinsBeforeAttemptAndAdvance5(child_ReplicationSchedulePinsBeforeAttemptAndAdvance50)->
	child_ReplicationSchedulePinsBeforeAttemptAndAdvance50?0->
stop_process:skip
}

proctype ReplicationSchedulePinsBeforeAttemptAndAdvance5(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance1383 = [1] of {int}->
	chan child_AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance1262 = [1] of {int}->
	chan child_AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance1181 = [1] of {int}->
	chan child_AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance1060 = [1] of {int}->
	Chandef advanced_ch->
	Chandef verified_ch->
	Chandef attempted_ch->
	Chandef pinned_ch->
	run sync_monitor(pinned_ch)->
	run sync_monitor(attempted_ch)->
	run sync_monitor(verified_ch)->
	run sync_monitor(advanced_ch)->
	run AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance106(pinned_ch,attempted_ch,verified_ch,advanced_ch,child_AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance1060)->
	run receiver(child_AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance1060)->
	run AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance118(verified_ch,advanced_ch,pinned_ch,attempted_ch,child_AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance1181)->
	run receiver(child_AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance1181)->
	run AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance126(pinned_ch,attempted_ch,verified_ch,advanced_ch,child_AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance1262)->
	run receiver(child_AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance1262)->
	run AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance138(verified_ch,advanced_ch,pinned_ch,attempted_ch,child_AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance1383)->
	run receiver(child_AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance1383)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:14:2 */
	:: advanced_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:14:2 */->
	:: advanced_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:14:2 */ -> 
		advanced_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance106(Chandef pinned_ch;Chandef attempted_ch;Chandef verified_ch;Chandef advanced_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:10:14 */
	:: pinned_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:10:14 */
	:: pinned_ch.sync!false -> 
		pinned_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:10:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance118(Chandef verified_ch;Chandef advanced_ch;Chandef pinned_ch;Chandef attempted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:11:14 */
	:: pinned_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:11:14 */->
	:: pinned_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:11:14 */ -> 
		pinned_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:11:24 */
	:: attempted_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:11:24 */
	:: attempted_ch.sync!false -> 
		attempted_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:11:24 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance126(Chandef pinned_ch;Chandef attempted_ch;Chandef verified_ch;Chandef advanced_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:12:14 */
	:: attempted_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:12:14 */->
	:: attempted_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:12:14 */ -> 
		attempted_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:12:27 */
	:: verified_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:12:27 */
	:: verified_ch.sync!false -> 
		verified_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:12:27 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousReplicationSchedulePinsBeforeAttemptAndAdvance138(Chandef verified_ch;Chandef advanced_ch;Chandef pinned_ch;Chandef attempted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:13:14 */
	:: verified_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:13:14 */->
	:: verified_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:13:14 */ -> 
		verified_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:13:26 */
	:: advanced_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:13:26 */
	:: advanced_ch.sync!false -> 
		advanced_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/replication_schedule.go:13:26 */
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

