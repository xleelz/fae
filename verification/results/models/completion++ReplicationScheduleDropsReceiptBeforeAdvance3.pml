// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go#L3
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
	chan child_ReplicationScheduleDropsReceiptBeforeAdvance30 = [1] of {int}->
	run ReplicationScheduleDropsReceiptBeforeAdvance3(child_ReplicationScheduleDropsReceiptBeforeAdvance30)->
	child_ReplicationScheduleDropsReceiptBeforeAdvance30?0->
stop_process:skip
}

proctype ReplicationScheduleDropsReceiptBeforeAdvance3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousReplicationScheduleDropsReceiptBeforeAdvance1173 = [1] of {int}->
	chan child_AnonymousReplicationScheduleDropsReceiptBeforeAdvance1042 = [1] of {int}->
	chan child_AnonymousReplicationScheduleDropsReceiptBeforeAdvance941 = [1] of {int}->
	chan child_AnonymousReplicationScheduleDropsReceiptBeforeAdvance850 = [1] of {int}->
	Chandef advanced_ch->
	Chandef verified_ch->
	Chandef attempted_ch->
	Chandef pinned_ch->
	run sync_monitor(pinned_ch)->
	run sync_monitor(attempted_ch)->
	run sync_monitor(verified_ch)->
	run sync_monitor(advanced_ch)->
	run AnonymousReplicationScheduleDropsReceiptBeforeAdvance85(attempted_ch,verified_ch,advanced_ch,pinned_ch,child_AnonymousReplicationScheduleDropsReceiptBeforeAdvance850)->
	run receiver(child_AnonymousReplicationScheduleDropsReceiptBeforeAdvance850)->
	run AnonymousReplicationScheduleDropsReceiptBeforeAdvance94(pinned_ch,attempted_ch,verified_ch,advanced_ch,child_AnonymousReplicationScheduleDropsReceiptBeforeAdvance941)->
	run receiver(child_AnonymousReplicationScheduleDropsReceiptBeforeAdvance941)->
	run AnonymousReplicationScheduleDropsReceiptBeforeAdvance104(pinned_ch,attempted_ch,verified_ch,advanced_ch,child_AnonymousReplicationScheduleDropsReceiptBeforeAdvance1042)->
	run receiver(child_AnonymousReplicationScheduleDropsReceiptBeforeAdvance1042)->
	run AnonymousReplicationScheduleDropsReceiptBeforeAdvance117(advanced_ch,pinned_ch,attempted_ch,verified_ch,child_AnonymousReplicationScheduleDropsReceiptBeforeAdvance1173)->
	run receiver(child_AnonymousReplicationScheduleDropsReceiptBeforeAdvance1173)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:12:2 */
	:: advanced_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:12:2 */->
	:: advanced_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:12:2 */ -> 
		advanced_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousReplicationScheduleDropsReceiptBeforeAdvance85(Chandef attempted_ch;Chandef verified_ch;Chandef advanced_ch;Chandef pinned_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:8:12 */
	:: pinned_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:8:12 */
	:: pinned_ch.sync!false -> 
		pinned_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:8:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousReplicationScheduleDropsReceiptBeforeAdvance94(Chandef pinned_ch;Chandef attempted_ch;Chandef verified_ch;Chandef advanced_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:9:12 */
	:: pinned_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:9:12 */->
	:: pinned_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:9:12 */ -> 
		pinned_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:9:21 */
	:: attempted_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:9:21 */
	:: attempted_ch.sync!false -> 
		attempted_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:9:21 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousReplicationScheduleDropsReceiptBeforeAdvance104(Chandef pinned_ch;Chandef attempted_ch;Chandef verified_ch;Chandef advanced_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:10:12 */
	:: attempted_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:10:12 */->
	:: attempted_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:10:12 */ -> 
		attempted_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:10:24 */
	:: verified_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:10:24 */
	:: verified_ch.sync!false -> 
		verified_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:10:24 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousReplicationScheduleDropsReceiptBeforeAdvance117(Chandef advanced_ch;Chandef pinned_ch;Chandef attempted_ch;Chandef verified_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:11:12 */
	:: advanced_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:11:12 */
	:: advanced_ch.sync!false -> 
		advanced_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/negative/replication_schedule.go:11:12 */
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

